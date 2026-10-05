package gitea

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ladder/ladder-agent/internal/protocol"
)

const (
	proxyTimeout = 30 * time.Second
	// Bodies travel base64-encoded in one WebSocket frame of at most 16 MiB.
	maxProxyBody = 8 << 20
)

var proxyMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true,
}

// Proxy forwards one git.http call from the cloud to the Gitea API (spec
// §5.5). Only paths of the GitOps repository pass, and the admin token is
// added here, so it never leaves the cluster. Gitea's answer is returned
// whatever its status.
func (i *Installer) Proxy(ctx context.Context, repo string, req protocol.GitHTTPRequest) (*protocol.GitHTTPResponse, error) {
	if repo == "" {
		return nil, protocol.Errorf(protocol.CodeFailedPrecondition, "setup has not created the repository yet")
	}
	if !proxyMethods[req.Method] {
		return nil, protocol.Errorf(protocol.CodeBadRequest, "method %q is not allowed", req.Method)
	}
	if !allowedPath(req.Path, "/api/v1/repos/"+Owner+"/"+repo) {
		return nil, protocol.Errorf(protocol.CodeBadRequest, "path %q is outside the repository %s/%s", req.Path, Owner, repo)
	}
	if _, err := url.ParseQuery(req.Query); err != nil {
		return nil, protocol.Errorf(protocol.CodeBadRequest, "invalid query: %v", err)
	}
	if len(req.Body) > maxProxyBody {
		return nil, protocol.Errorf(protocol.CodeBadRequest, "body is larger than %d bytes", maxProxyBody)
	}
	creds, err := i.Credentials(ctx)
	if err != nil {
		return nil, err
	}
	if creds == nil || creds.Token == "" {
		return nil, protocol.Errorf(protocol.CodeFailedPrecondition, "Gitea is not set up yet")
	}

	target := i.baseURL + req.Path
	if req.Query != "" {
		target += "?" + req.Query
	}
	ctx, cancel := context.WithTimeout(ctx, proxyTimeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, req.Method, target, bytes.NewReader(req.Body))
	if err != nil {
		return nil, protocol.Errorf(protocol.CodeBadRequest, "%v", err)
	}
	hreq.Header.Set("Authorization", "token "+creds.Token)
	hreq.Header.Set("Accept", "application/json")
	if len(req.Body) > 0 {
		hreq.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProxyBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxProxyBody {
		return nil, protocol.Errorf(protocol.CodeInternal, "Gitea answer is larger than %d bytes", maxProxyBody)
	}
	return &protocol.GitHTTPResponse{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: body}, nil
}

// allowedPath reports whether the escaped path is prefix itself or below it,
// with no ".", ".." or empty segments, and no query or fragment.
func allowedPath(escaped, prefix string) bool {
	if strings.ContainsAny(escaped, "?#") {
		return false
	}
	p, err := url.PathUnescape(escaped)
	if err != nil {
		return false
	}
	if p == prefix {
		return true
	}
	rest, ok := strings.CutPrefix(p, prefix+"/")
	if !ok {
		return false
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
