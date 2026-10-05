package gitea

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ladder/ladder-agent/internal/kube"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestHookHandler(t *testing.T) {
	const secret = "s3cret"
	k := &kube.Clients{Kube: fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: AdminSecret, Namespace: Namespace},
		Data:       map[string][]byte{"webhook-secret": []byte(secret)},
	})}
	i := NewInstaller(k, "", "", "http://ladder-agent.ladder.svc"+HookPath, slog.Default())
	sign := func(body string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		return hex.EncodeToString(mac.Sum(nil))
	}
	const toMain = `{"ref":"refs/heads/main","after":"3daf9d1"}`
	for _, tc := range []struct {
		name, event, body, signature string
		status                       int
		pushed                       bool
	}{
		{"push to main", "push", toMain, sign(toMain), http.StatusNoContent, true},
		{"bad signature", "push", toMain, sign("other"), http.StatusUnauthorized, false},
		{"no signature", "push", toMain, "", http.StatusUnauthorized, false},
		{"other branch", "push", `{"ref":"refs/heads/ladder/x"}`, sign(`{"ref":"refs/heads/ladder/x"}`), http.StatusNoContent, false},
		{"other event", "create", toMain, sign(toMain), http.StatusNoContent, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pushed := false
			h := i.HookHandler(func(context.Context) error { pushed = true; return nil })
			r := httptest.NewRequest(http.MethodPost, HookPath, strings.NewReader(tc.body))
			r.Header.Set("X-Gitea-Event", tc.event)
			r.Header.Set("X-Gitea-Signature", tc.signature)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || pushed != tc.pushed {
				t.Errorf("status %d, pushed %v; want %d, %v", w.Code, pushed, tc.status, tc.pushed)
			}
		})
	}
}
