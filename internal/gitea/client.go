// Package gitea installs the in-cluster git server for Local mode and talks
// to its REST API.
package gitea

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"
)

// Client is a minimal Gitea API client. Admin calls use the token; creating
// the token itself uses basic auth.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("gitea: HTTP %d: %s", e.Status, e.Body) }

func (c *Client) do(ctx context.Context, method, path string, body any, basic *url.Userinfo, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/api/v1"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if basic != nil {
		pw, _ := basic.Password()
		req.SetBasicAuth(basic.Username(), pw)
	} else if c.Token != "" {
		req.Header.Set("Authorization", "token "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &apiError{Status: resp.StatusCode, Body: string(b)}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func isStatus(err error, codes ...int) bool {
	e, ok := err.(*apiError)
	if !ok {
		return false
	}
	for _, c := range codes {
		if e.Status == c {
			return true
		}
	}
	return false
}

// Healthy reports whether the API answers.
func (c *Client) Healthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// CreateToken creates an access token for user with basic auth.
func (c *Client) CreateToken(ctx context.Context, user, password, name string) (string, error) {
	var out struct {
		SHA1 string `json:"sha1"`
	}
	err := c.do(ctx, http.MethodPost, "/users/"+user+"/tokens", map[string]any{
		"name":   name,
		"scopes": []string{"write:repository", "write:issue", "write:user", "write:admin"},
	}, url.UserPassword(user, password), &out)
	return out.SHA1, err
}

// EnsureUser creates a user; an existing user is fine.
func (c *Client) EnsureUser(ctx context.Context, name, password, email string) error {
	err := c.do(ctx, http.MethodPost, "/admin/users", map[string]any{
		"username":             name,
		"password":             password,
		"email":                email,
		"must_change_password": false,
	}, nil, nil)
	if isStatus(err, http.StatusUnprocessableEntity) {
		return nil
	}
	return err
}

// EnsureRepo creates a public repository of the token's user, owner; an
// existing one is made public. Public means people clone it without signing in.
func (c *Client) EnsureRepo(ctx context.Context, owner, name string) error {
	err := c.do(ctx, http.MethodPost, "/user/repos", map[string]any{
		"name":           name,
		"private":        false,
		"default_branch": "main",
		"auto_init":      false,
	}, nil, nil)
	if isStatus(err, http.StatusConflict) {
		return c.do(ctx, http.MethodPatch, "/repos/"+owner+"/"+name, map[string]any{"private": false}, nil, nil)
	}
	return err
}

func (c *Client) RepoExists(ctx context.Context, owner, repo string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/repos/"+owner+"/"+repo, nil, nil, nil)
	if isStatus(err, http.StatusNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (c *Client) AddCollaborator(ctx context.Context, owner, repo, user, permission string) error {
	return c.do(ctx, http.MethodPut, "/repos/"+owner+"/"+repo+"/collaborators/"+user,
		map[string]any{"permission": permission}, nil, nil)
}

func (c *Client) FileExists(ctx context.Context, owner, repo, path, ref string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/repos/"+owner+"/"+repo+"/contents/"+path+"?ref="+url.QueryEscape(ref), nil, nil, nil)
	if isStatus(err, http.StatusNotFound) {
		return false, nil
	}
	return err == nil, err
}

// CommitFiles creates all files in one commit on branch. Works on an empty repository.
func (c *Client) CommitFiles(ctx context.Context, owner, repo, branch, message string, files map[string]string) error {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	ops := make([]map[string]string, 0, len(paths))
	for _, p := range paths {
		ops = append(ops, map[string]string{
			"operation": "create",
			"path":      p,
			"content":   base64.StdEncoding.EncodeToString([]byte(files[p])),
		})
	}
	return c.do(ctx, http.MethodPost, "/repos/"+owner+"/"+repo+"/contents", map[string]any{
		"branch":  branch,
		"message": message,
		"files":   ops,
	}, nil, nil)
}
