// Package worker runs the consumer side of playscape: a loop that leases
// tasks from a queue, executes them on a node, and reports results. The
// control plane runs workers as goroutines for local scaling; `playscape
// work` runs the same loop in its own process, which is what Kubernetes
// deployments and serverless containers replicate to scale out.
package worker

import (
	"context"
	"errors"
	"time"

	"github.com/urmzd/playscape/pkg/metrics"
	"github.com/urmzd/playscape/pkg/node"
	"github.com/urmzd/playscape/pkg/queue"
	"github.com/urmzd/playscape/pkg/task"
)

// Worker consumes tasks for one deployment on one node.
type Worker struct {
	// Deployment names the deployment, for metric labels.
	Deployment string
	// Queue supplies tasks; competing workers share it.
	Queue queue.Queue
	// Results receives each task's outcome.
	Results queue.Results
	// Node executes the tasks.
	Node node.Node
	// Recorder receives task metrics. Nil disables recording.
	Recorder metrics.Recorder
	// Backoff is the pause after a transient dequeue failure (e.g. the
	// control plane briefly unreachable). Zero means one second.
	Backoff time.Duration
}

// Run consumes tasks until ctx is done, which is the only error it returns.
// Task failures are reported as results, not returned: a bad task must never
// take the consumer down with it.
func (w *Worker) Run(ctx context.Context) error {
	rec := w.Recorder
	if rec == nil {
		rec = metrics.Nop()
	}
	backoff := w.Backoff
	if backoff <= 0 {
		backoff = time.Second
	}

	for {
		t, err := w.Queue.Dequeue(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Transient: queue unreachable. Pause and retry.
			select {
			case <-time.After(backoff):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		labels := []metrics.Label{
			{Key: "deployment", Value: w.Deployment},
			{Key: "tool", Value: t.Tool},
		}
		start := time.Now()
		res, err := w.Node.Run(ctx, t)
		rec.Observe("playscape_task_seconds", time.Since(start).Seconds(), labels...)
		status := "ok"
		if err != nil {
			status = "error"
			res = task.Result{TaskID: t.ID, NodeID: w.Node.ID(), Error: err.Error()}
		}
		rec.Count("playscape_tasks_total", 1, append(labels, metrics.Label{Key: "status", Value: status})...)

		if err := w.Results.Report(ctx, res); err != nil && !errors.Is(err, context.Canceled) {
			rec.Count("playscape_result_report_failures_total", 1, labels...)
		}
	}
}
