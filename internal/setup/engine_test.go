package setup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"testing"
	"time"
)

func TestRunRetrying(t *testing.T) {
	firstRetryPause = time.Millisecond
	dropped := &url.Error{Op: "Patch", URL: "https://10.110.32.1:443/api/v1/namespaces/argocd/serviceaccounts/x", Err: io.ErrUnexpectedEOF}
	e := &Engine{log: slog.New(slog.DiscardHandler)}
	// failing makes a step that fails with errs, one per run, then succeeds.
	failing := func(runs *int, errs ...error) step {
		return step{id: "s", run: func(context.Context) error {
			*runs++
			if *runs <= len(errs) {
				return errs[*runs-1]
			}
			return nil
		}}
	}

	t.Run("dropped connection is run again", func(t *testing.T) {
		runs := 0
		if err := e.runRetrying(context.Background(), failing(&runs, dropped, dropped)); err != nil {
			t.Fatal(err)
		}
		if runs != 3 {
			t.Errorf("runs = %d, want 3", runs)
		}
	})

	t.Run("work that went through is done", func(t *testing.T) {
		runs := 0
		s := failing(&runs, dropped)
		s.done = func(context.Context) (bool, error) { return true, nil }
		if err := e.runRetrying(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		if runs != 1 {
			t.Errorf("runs = %d, want 1", runs)
		}
	})

	t.Run("refusal fails at once", func(t *testing.T) {
		runs := 0
		refused := errors.New("the Argo CD CRDs are installed")
		if err := e.runRetrying(context.Background(), failing(&runs, refused)); err != refused {
			t.Fatalf("err = %v, want %v", err, refused)
		}
		if runs != 1 {
			t.Errorf("runs = %d, want 1", runs)
		}
	})

	t.Run("gives up when the step's time is over", func(t *testing.T) {
		runs := 0
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		s := step{id: "s", run: func(context.Context) error { runs++; return dropped }}
		if err := e.runRetrying(ctx, s); err != dropped {
			t.Fatalf("err = %v, want the last error %v", err, dropped)
		}
		if runs < 2 {
			t.Errorf("runs = %d, want at least 2", runs)
		}
	})
}
