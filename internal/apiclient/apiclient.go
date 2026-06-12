// Package apiclient is a small HTTP client for the proxy's REST API, used by
// companion tools such as the tray app. It mirrors the JSON shapes the API
// emits without importing the server packages.
package apiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client talks to a running proxy's HTTP API.
type Client struct {
	base  string
	http  *http.Client
	user  string
	token string
}

// New returns a client for the API at baseURL (e.g. "http://127.0.0.1:8420").
func New(baseURL string) *Client {
	return &Client{base: baseURL, http: &http.Client{Timeout: 5 * time.Second}}
}

// NewWithAuth returns a client that sends HTTP Basic Auth when token is set.
func NewWithAuth(baseURL, user, token string) *Client {
	c := New(baseURL)
	c.user = user
	c.token = token
	return c
}

// MachineStatus mirrors service.MachineStatus.
type MachineStatus struct {
	State     string `json:"state"`
	Mode      string `json:"mode"`
	Connected bool   `json:"connected"`
	AgeMs     int64  `json:"age_ms"`
}

// Job mirrors the fields of store.Job the tray cares about.
type Job struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	State     string `json:"state"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error"`
}

// File mirrors the catalog fields the tray cares about.
type File struct {
	Path string `json:"path"`
	Sync string `json:"sync"`
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		user := c.user
		if user == "" {
			user = "cnc"
		}
		req.SetBasicAuth(user, c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("apiclient: GET %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Machine fetches the current machine status.
func (c *Client) Machine(ctx context.Context) (MachineStatus, error) {
	var m MachineStatus
	err := c.getJSON(ctx, "/api/machine", &m)
	return m, err
}

// Jobs fetches the job queue.
func (c *Client) Jobs(ctx context.Context) ([]Job, error) {
	var j []Job
	err := c.getJSON(ctx, "/api/jobs", &j)
	return j, err
}

// Files fetches the catalog.
func (c *Client) Files(ctx context.Context) ([]File, error) {
	var f []File
	err := c.getJSON(ctx, "/api/files", &f)
	return f, err
}

// PendingCount returns how many catalog entries are not yet synced.
func PendingCount(files []File) int {
	n := 0
	for _, f := range files {
		if f.Sync != "synced" && f.Sync != "remote_only" {
			n++
		}
	}
	return n
}
