package gitea

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
)

// HookPath is where the agent receives Gitea's push webhook.
const HookPath = "/hooks/git"

const maxHookBody = 8 << 20

// HookHandler receives the push webhook that Install creates. It checks the
// signature against the secret in Secret ladder-git-admin and calls onPush for
// pushes to main. An error from onPush answers 500, which Gitea keeps in the
// webhook's delivery log.
func (i *Installer) HookHandler(onPush func(context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBody))
		if err != nil {
			http.Error(w, "cannot read the body", http.StatusBadRequest)
			return
		}
		creds, err := i.Credentials(r.Context())
		if err != nil || creds == nil || creds.HookSecret == "" {
			http.Error(w, "the webhook secret is not set up", http.StatusServiceUnavailable)
			return
		}
		if !validSignature(creds.HookSecret, body, r.Header.Get("X-Gitea-Signature")) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		var push struct {
			Ref string `json:"ref"`
		}
		if r.Header.Get("X-Gitea-Event") != "push" || json.Unmarshal(body, &push) != nil || push.Ref != "refs/heads/main" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := onPush(r.Context()); err != nil {
			i.log.Error("handle push", "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// validSignature checks X-Gitea-Signature: the hex HMAC-SHA256 of the body.
func validSignature(secret string, body []byte, signature string) bool {
	got, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}
