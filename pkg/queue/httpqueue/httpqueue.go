// Package httpqueue implements queue.Queue and queue.Results over the
// playscape control plane's HTTP API. It is what remote consumers — the
// `playscape work` process that Kubernetes pods and serverless containers
// replicate — use to compete for tasks on a deployment's queue, and what
// their tools spawn sub-tasks through. No broker is required: the control
// plane service is the rendezvous point.
package httpqueue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/urmzd/playscape/pkg/controlplane"
	"github.com/urmzd/playscape/pkg/task"
)

// Client speaks to one deployment on one control plane server.
type Client struct {
	base       string
	deployment string
	http       *http.Client
	// LeaseWait is the long-poll duration per lease request (default 30s).
	LeaseWait time.Duration
	// PollInterval is the result polling cadence for Await (default 500ms).
	PollInterval time.Duration
}

// New returns a client for deployment on the control plane at baseURL
// (e.g. "http://playscape:8484").
func New(baseURL, deployment string) *Client {
	return &Client{
		base:       strings.TrimRight(baseURL, "/"),
		deployment: deployment,
		http:       &http.Client{Timeout: 90 * time.Second},
	}
}

func (c *Client) url(suffix string) string {
	return fmt.Sprintf("%s/v1/deployments/%s%s", c.base, c.deployment, suffix)
}

// SubmitAsync enqueues t on the deployment and returns its task ID. It is
// the Spawner remote nodes hand to their tools.
func (c *Client) SubmitAsync(ctx context.Context, t task.Task) (string, error) {
	body, err := json.Marshal(map[string]any{
		"id":    t.ID,
		"tool":  t.Tool,
		"input": string(t.Input),
		"async": true,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		TaskID string `json:"task_id"`
	}
	if err := c.do(ctx, http.MethodPost, c.url("/tasks"), body, &out); err != nil {
		return "", fmt.Errorf("httpqueue: submit: %w", err)
	}
	return out.TaskID, nil
}

// Enqueue implements queue.Queue.
func (c *Client) Enqueue(ctx context.Context, t task.Task) error {
	_, err := c.SubmitAsync(ctx, t)
	return err
}

// Dequeue implements queue.Queue by long-polling the deployment's lease
// endpoint until a task arrives or ctx is done.
func (c *Client) Dequeue(ctx context.Context) (task.Task, error) {
	wait := c.LeaseWait
	if wait <= 0 {
		wait = 30 * time.Second
	}
	for {
		if err := ctx.Err(); err != nil {
			return task.Task{}, err
		}
		url := fmt.Sprintf("%s?wait=%ds", c.url("/lease"), int(wait.Seconds()))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			return task.Task{}, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return task.Task{}, fmt.Errorf("httpqueue: lease: %w", err)
		}
		switch resp.StatusCode {
		case http.StatusOK:
			var t task.Task
			err := json.NewDecoder(resp.Body).Decode(&t)
			resp.Body.Close()
			if err != nil {
				return task.Task{}, fmt.Errorf("httpqueue: decode lease: %w", err)
			}
			return t, nil
		case http.StatusNoContent:
			resp.Body.Close() // empty long poll; lease again
		default:
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			return task.Task{}, fmt.Errorf("httpqueue: lease: %s: %s", resp.Status, bytes.TrimSpace(msg))
		}
	}
}

// Report implements queue.Results.
func (c *Client) Report(ctx context.Context, r task.Result) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := c.do(ctx, http.MethodPost, c.url("/results"), body, nil); err != nil {
		return fmt.Errorf("httpqueue: report: %w", err)
	}
	return nil
}

// Get implements queue.Results.
func (c *Client) Get(ctx context.Context, taskID string) (task.Result, bool, error) {
	var out struct {
		Status string      `json:"status"`
		Result task.Result `json:"result"`
	}
	if err := c.do(ctx, http.MethodGet, c.url("/tasks/"+taskID), nil, &out); err != nil {
		return task.Result{}, false, fmt.Errorf("httpqueue: result: %w", err)
	}
	return out.Result, out.Status == "done", nil
}

// Await implements queue.Results by polling Get.
func (c *Client) Await(ctx context.Context, taskID string) (task.Result, error) {
	interval := c.PollInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	for {
		r, done, err := c.Get(ctx, taskID)
		if err != nil {
			return task.Result{}, err
		}
		if done {
			return r, nil
		}
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return task.Result{}, ctx.Err()
		}
	}
}

// Spec fetches the deployment's service spec so a remote consumer can
// reconstruct its sandbox policies.
func (c *Client) Spec(ctx context.Context) (controlplane.ServiceSpec, error) {
	var spec controlplane.ServiceSpec
	if err := c.do(ctx, http.MethodGet, c.url("/spec"), nil, &spec); err != nil {
		return controlplane.ServiceSpec{}, fmt.Errorf("httpqueue: spec: %w", err)
	}
	return spec, nil
}

func (c *Client) do(ctx context.Context, method, url string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
