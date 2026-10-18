// Package server exposes a ControlPlane over HTTP. It is a thin JSON
// translation layer: all behavior lives in the control plane it wraps. The
// same API serves both sides of the queue — producers submit tasks and read
// results; remote consumers lease work and report outcomes.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/urmzd/playscape/pkg/controlplane"
	"github.com/urmzd/playscape/pkg/metrics"
	"github.com/urmzd/playscape/pkg/task"
)

// New returns an http.Handler serving the beta v1 API.
//
// Producer side:
//
//	GET  /healthz                          liveness probe
//	GET  /metrics                          current metric values (text)
//	GET  /v1/deployments                   list deployments
//	POST /v1/deployments                   deploy a service (ServiceSpec JSON)
//	GET  /v1/deployments/{name}            one deployment's status
//	GET  /v1/deployments/{name}/spec       the deployment's ServiceSpec
//	POST /v1/deployments/{name}/scale      {"replicas": N} (local nodes)
//	POST /v1/deployments/{name}/tasks      {"tool","input","async"} → result, or task_id if async
//	GET  /v1/deployments/{name}/tasks/{id} task result: {"status","result"}
//
// Consumer side (what `playscape work` replicas call):
//
//	POST /v1/deployments/{name}/lease?wait=30s  long-poll for a task (204 when empty)
//	POST /v1/deployments/{name}/results         report a task result
func New(cp controlplane.ControlPlane, consumer controlplane.Consumer, mem *metrics.Memory) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		snap := mem.Snapshot()
		keys := make([]string, 0, len(snap))
		for k := range snap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "%s %g\n", k, snap[k])
		}
	})

	mux.HandleFunc("GET /v1/deployments", func(w http.ResponseWriter, r *http.Request) {
		list, err := cp.List(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	})

	mux.HandleFunc("POST /v1/deployments", func(w http.ResponseWriter, r *http.Request) {
		var spec controlplane.ServiceSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := cp.Deploy(r.Context(), spec); err != nil {
			writeErr(w, err)
			return
		}
		status, err := cp.Status(r.Context(), spec.Name)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, status)
	})

	mux.HandleFunc("GET /v1/deployments/{name}", func(w http.ResponseWriter, r *http.Request) {
		status, err := cp.Status(r.Context(), r.PathValue("name"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	})

	mux.HandleFunc("GET /v1/deployments/{name}/spec", func(w http.ResponseWriter, r *http.Request) {
		spec, err := cp.Spec(r.Context(), r.PathValue("name"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, spec)
	})

	mux.HandleFunc("POST /v1/deployments/{name}/scale", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Replicas int `json:"replicas"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		name := r.PathValue("name")
		if err := cp.Scale(r.Context(), name, req.Replicas); err != nil {
			writeErr(w, err)
			return
		}
		status, err := cp.Status(r.Context(), name)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	})

	mux.HandleFunc("POST /v1/deployments/{name}/tasks", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID    string `json:"id,omitempty"`
			Tool  string `json:"tool"`
			Input string `json:"input,omitempty"`
			Async bool   `json:"async,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		name := r.PathValue("name")
		t := task.Task{ID: req.ID, Tool: req.Tool, Input: []byte(req.Input)}

		if req.Async {
			id, err := cp.SubmitAsync(r.Context(), name, t)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusAccepted, map[string]string{"task_id": id})
			return
		}

		res, err := cp.Submit(r.Context(), name, t)
		if err != nil && res.Error == "" {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, resultJSON(res))
	})

	mux.HandleFunc("GET /v1/deployments/{name}/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		res, done, err := cp.Result(r.Context(), r.PathValue("name"), r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		if !done {
			writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "done", "result": res})
	})

	mux.HandleFunc("POST /v1/deployments/{name}/lease", func(w http.ResponseWriter, r *http.Request) {
		wait := 30 * time.Second
		if q := r.URL.Query().Get("wait"); q != "" {
			if d, err := time.ParseDuration(q); err == nil && d > 0 && d <= time.Minute {
				wait = d
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), wait)
		defer cancel()

		t, err := consumer.Lease(ctx, r.PathValue("name"))
		if err != nil {
			if ctx.Err() != nil {
				w.WriteHeader(http.StatusNoContent) // empty long poll
				return
			}
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, t)
	})

	mux.HandleFunc("POST /v1/deployments/{name}/results", func(w http.ResponseWriter, r *http.Request) {
		var res task.Result
		if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if res.TaskID == "" {
			http.Error(w, "task_id required", http.StatusBadRequest)
			return
		}
		if err := consumer.Report(r.Context(), r.PathValue("name"), res); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

// resultJSON renders a Result with input/output as plain strings for curl
// friendliness (task.Result marshals Output as base64).
func resultJSON(res task.Result) map[string]string {
	out := map[string]string{
		"task_id": res.TaskID,
		"node_id": res.NodeID,
		"output":  string(res.Output),
	}
	if res.Error != "" {
		out["error"] = res.Error
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v) //nolint:errcheck // response already committed
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, controlplane.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, controlplane.ErrExists):
		code = http.StatusConflict
	}
	http.Error(w, err.Error(), code)
}
