// Command fakebe is a stand-in for ladder-be during agent development. It
// accepts the agent's WebSocket, prints its events, and turns plain HTTP calls
// into protocol requests:
//
//	curl localhost:9000/cluster
//	curl localhost:9000/apps
//	curl -X POST localhost:9000/setup/plan  -d '{"provider":"local"}'
//	curl -X POST localhost:9000/setup/apply -d '{"provider":"local"}'
//	curl localhost:9000/setup/status
//	curl -X POST localhost:9000/setup/retry
//	curl localhost:9000/argocd/credentials
//	curl -X POST localhost:9000/kube -d '{"op":"list","kind":"pods","namespace":"argocd"}'
//	curl -X POST localhost:9000/git -d '{"method":"GET","path":"/api/v1/repos/ladder/deploy/contents/ladder.yaml"}'
//
// For setup.apply it renders a minimal starter repository from ./starter.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/coder/websocket"
	"github.com/ladder/ladder-agent/internal/protocol"
)

//go:embed all:starter
var starterFS embed.FS

func main() {
	addr := flag.String("addr", ":9000", "listen address")
	token := flag.String("token", envOr("LADDER_AGENT_TOKEN", "dev-token"), "token the agent must present")
	flag.Parse()

	b := &backend{token: *token, pending: map[string]chan protocol.Envelope{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/connect", b.connect)
	mux.HandleFunc("GET /cluster", b.forward(protocol.MethodClusterInfo))
	mux.HandleFunc("GET /apps", b.forward(protocol.MethodAppsStatus))
	mux.HandleFunc("GET /setup/status", b.forward(protocol.MethodSetupStatus))
	mux.HandleFunc("POST /setup/plan", b.forward(protocol.MethodSetupPlan))
	mux.HandleFunc("POST /setup/retry", b.forward(protocol.MethodSetupRetry))
	mux.HandleFunc("POST /setup/apply", b.apply)
	mux.HandleFunc("GET /argocd/credentials", b.forward(protocol.MethodArgoCDCredentials))
	mux.HandleFunc("POST /git", b.forward(protocol.MethodGitHTTP))
	mux.HandleFunc("POST /kube", b.forward(protocol.MethodKubeRead))
	log.Printf("fakebe listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

type backend struct {
	token string

	mu      sync.Mutex
	conn    *websocket.Conn
	pending map[string]chan protocol.Envelope
}

func (b *backend) connect(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+b.token {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(16 << 20)
	b.mu.Lock()
	if b.conn != nil {
		b.conn.Close(websocket.StatusGoingAway, "replaced by a new agent connection")
	}
	b.conn = conn
	b.mu.Unlock()
	log.Printf("agent connected from %s", r.RemoteAddr)

	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			log.Printf("agent disconnected: %v", err)
			b.mu.Lock()
			if b.conn == conn {
				b.conn = nil
			}
			b.mu.Unlock()
			return
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			log.Printf("bad message: %v", err)
			continue
		}
		switch env.Kind {
		case protocol.KindResponse:
			b.mu.Lock()
			ch := b.pending[env.ID]
			delete(b.pending, env.ID)
			b.mu.Unlock()
			if ch != nil {
				ch <- env
			}
		case protocol.KindEvent:
			printEvent(env)
		}
	}
}

func (b *backend) call(ctx context.Context, method string, payload json.RawMessage) (protocol.Envelope, error) {
	id := randomID()
	ch := make(chan protocol.Envelope, 1)
	b.mu.Lock()
	conn := b.conn
	if conn != nil {
		b.pending[id] = ch
	}
	b.mu.Unlock()
	if conn == nil {
		return protocol.Envelope{}, errors.New("agent is not connected")
	}
	defer func() {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
	}()
	msg, _ := json.Marshal(protocol.Envelope{ID: id, Kind: protocol.KindRequest, Method: method, Payload: payload})
	if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
		return protocol.Envelope{}, err
	}
	select {
	case env := <-ch:
		return env, nil
	case <-ctx.Done():
		return protocol.Envelope{}, ctx.Err()
	}
}

func (b *backend) forward(method string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if len(bytes.TrimSpace(body)) == 0 {
			body = nil
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		env, err := b.call(ctx, method, body)
		reply(w, env, err)
	}
}

// apply asks the agent for the plan (to learn the repository URL and Argo CD
// choice), renders the starter repository, then sends setup.apply.
func (b *backend) apply(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	var plan protocol.SetupPlan
	if err := json.NewDecoder(r.Body).Decode(&plan); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw, _ := json.Marshal(plan)
	env, err := b.call(ctx, protocol.MethodSetupPlan, raw)
	if err != nil || env.Error != nil {
		reply(w, env, err)
		return
	}
	var result protocol.SetupPlanResult
	if err := json.Unmarshal(env.Payload, &result); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if plan.WriteMode == "" {
		plan.WriteMode = protocol.WriteModeCommit
	}
	files, err := renderStarter(map[string]any{
		"RepoURL":            result.RepoURL,
		"ArgoCDNamespace":    result.ArgoCD.Namespace,
		"ArgoCDVersion":      result.ArgoCD.Version,
		"ArgoCDChartVersion": result.ArgoCD.ChartVersion,
		"Exposure":           result.ArgoCD.Exposure,
		"Host":               result.ArgoCD.Host,
		"WriteMode":          plan.WriteMode,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	plan.Files = files
	raw, _ = json.Marshal(plan)
	env, err = b.call(ctx, protocol.MethodSetupApply, raw)
	reply(w, env, err)
}

func renderStarter(data map[string]any) (map[string]string, error) {
	files := map[string]string{}
	err := fs.WalkDir(starterFS, "starter", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		src, err := starterFS.ReadFile(path)
		if err != nil {
			return err
		}
		t, err := template.New(path).Delims("[[", "]]").Option("missingkey=error").Parse(string(src))
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, data); err != nil {
			return err
		}
		files[strings.TrimPrefix(path, "starter/")] = buf.String()
		return nil
	})
	return files, err
}

func reply(w http.ResponseWriter, env protocol.Envelope, err error) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case err != nil:
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "agent_unavailable", "message": err.Error()}})
	case env.Error != nil:
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]any{"error": env.Error})
	default:
		var out bytes.Buffer
		if json.Indent(&out, env.Payload, "", "  ") != nil {
			out.Write(env.Payload)
		}
		out.WriteByte('\n')
		w.Write(out.Bytes())
	}
}

func printEvent(env protocol.Envelope) {
	if env.Method == protocol.EventSetupStatus {
		var s protocol.SetupStatus
		if json.Unmarshal(env.Payload, &s) == nil {
			var b strings.Builder
			fmt.Fprintf(&b, "event %s: %s", env.Method, s.State)
			for _, st := range s.Steps {
				fmt.Fprintf(&b, "\n    %-16s %-8s %s", st.ID, st.Status, firstLine(st.Error+" "+st.Detail))
			}
			log.Print(b.String())
			return
		}
	}
	p := string(env.Payload)
	if len(p) > 600 {
		p = p[:600] + "…"
	}
	log.Printf("event %s: %s", env.Method, p)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
