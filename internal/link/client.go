// Package link keeps the agent's outbound WebSocket connection to ladder-be:
// it reconnects with backoff, dispatches requests to handlers and pushes events.
package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/ladder/ladder-agent/internal/protocol"
)

const (
	readLimit    = 16 << 20 // starter repository files arrive in one setup.apply
	pingInterval = 20 * time.Second
	pingTimeout  = 10 * time.Second
	writeTimeout = 10 * time.Second
	outBuffer    = 256
	minBackoff   = time.Second
	maxBackoff   = 30 * time.Second
)

// Handler serves one request method. Returning a *protocol.Error keeps its code.
type Handler func(ctx context.Context, payload json.RawMessage) (any, error)

type Client struct {
	url   string
	token string
	log   *slog.Logger

	handlers  map[string]Handler
	onConnect []func()

	session atomic.Pointer[session]
}

func New(url, token string, log *slog.Logger) *Client {
	return &Client{url: url, token: token, log: log, handlers: map[string]Handler{}}
}

// Handle registers a request handler. Call before Run.
func (c *Client) Handle(method string, h Handler) { c.handlers[method] = h }

// OnConnect registers a hook that runs after every successful connect, in
// registration order. Hooks usually Emit the current state.
func (c *Client) OnConnect(f func()) { c.onConnect = append(c.onConnect, f) }

// Connected reports whether a session is open.
func (c *Client) Connected() bool { return c.session.Load() != nil }

// Emit pushes an event. It never blocks: while disconnected the event is
// dropped, since every connect starts with fresh snapshots.
func (c *Client) Emit(event string, payload any) {
	s := c.session.Load()
	if s == nil {
		return
	}
	env, err := envelope(protocol.KindEvent, "", event, payload, nil)
	if err != nil {
		c.log.Error("encode event", "event", event, "err", err)
		return
	}
	select {
	case s.out <- env:
	default:
		c.log.Warn("event dropped, send buffer full", "event", event)
	}
}

// Run connects and serves until ctx is done.
func (c *Client) Run(ctx context.Context) {
	backoff := minBackoff
	for {
		connected, err := c.serve(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = minBackoff
		}
		c.log.Warn("backend connection lost", "err", err, "retryIn", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff + rand.N(backoff/2)):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

type session struct {
	out chan []byte
}

func (c *Client) serve(ctx context.Context) (connected bool, err error) {
	conn, _, err := websocket.Dial(ctx, c.url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.token}},
	})
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(readLimit)
	c.log.Info("connected to backend", "url", c.url)

	// Deferred in reverse: cancel the session, then wait for its goroutines.
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	s := &session{out: make(chan []byte, outBuffer)}
	wg.Go(func() { c.writeLoop(ctx, cancel, conn, s) })
	wg.Go(func() { c.pingLoop(ctx, cancel, conn) })

	c.session.Store(s)
	defer c.session.Store(nil)
	for _, f := range c.onConnect {
		f()
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
				err = cause
			}
			return true, err
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			c.log.Warn("bad message from backend", "err", err)
			continue
		}
		if env.Kind != protocol.KindRequest {
			continue
		}
		wg.Go(func() { c.dispatch(ctx, s, env) })
	}
}

func (c *Client) dispatch(ctx context.Context, s *session, req protocol.Envelope) {
	start := time.Now()
	var (
		result any
		perr   *protocol.Error
	)
	if h, ok := c.handlers[req.Method]; !ok {
		perr = protocol.Errorf(protocol.CodeUnknownMethod, "unknown method %q", req.Method)
	} else if res, err := h(ctx, req.Payload); err != nil {
		perr = protocol.AsError(err)
	} else {
		result = res
	}
	log := c.log.With("method", req.Method, "id", req.ID, "ms", time.Since(start).Milliseconds())
	if perr != nil {
		log.Warn("request failed", "code", perr.Code, "err", perr.Message)
	} else {
		log.Info("request served")
	}
	env, err := envelope(protocol.KindResponse, req.ID, req.Method, result, perr)
	if err != nil {
		env, _ = envelope(protocol.KindResponse, req.ID, req.Method, nil, protocol.AsError(err))
	}
	// Responses are not dropped: wait for room in the buffer.
	select {
	case s.out <- env:
	case <-ctx.Done():
	}
}

func (c *Client) writeLoop(ctx context.Context, cancel context.CancelCauseFunc, conn *websocket.Conn, s *session) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-s.out:
			wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
			err := conn.Write(wctx, websocket.MessageText, msg)
			wcancel()
			if err != nil {
				cancel(fmt.Errorf("write: %w", err))
				return
			}
		}
	}
}

func (c *Client) pingLoop(ctx context.Context, cancel context.CancelCauseFunc, conn *websocket.Conn) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, pcancel := context.WithTimeout(ctx, pingTimeout)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				cancel(fmt.Errorf("ping: %w", err))
				return
			}
		}
	}
}

func envelope(kind protocol.Kind, id, method string, payload any, perr *protocol.Error) ([]byte, error) {
	env := protocol.Envelope{ID: id, Kind: kind, Method: method, Error: perr}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		env.Payload = raw
	}
	return json.Marshal(env)
}
