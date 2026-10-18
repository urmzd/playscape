package controlplane

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/urmzd/playscape/pkg/metrics"
	"github.com/urmzd/playscape/pkg/node"
	"github.com/urmzd/playscape/pkg/queue"
	"github.com/urmzd/playscape/pkg/sandbox"
	"github.com/urmzd/playscape/pkg/task"
	"github.com/urmzd/playscape/pkg/worker"
)

// Memory is an in-process ControlPlane and Consumer host. Deployment state,
// queues, and results live in memory; local nodes are worker goroutines
// competing on each deployment's queue, and remote consumers lease from the
// same queues via Lease/Report. Node creation is delegated to a
// node.Factory, so the execution substrate can change without touching it.
type Memory struct {
	factory node.Factory
	rec     metrics.Recorder

	mu          sync.Mutex
	deployments map[string]*deployment
}

type deployment struct {
	spec    ServiceSpec
	pdp     sandbox.PDP
	queue   queue.Queue
	results queue.Results
	workers []*localWorker
}

// localWorker is one local consumer: a node plus the goroutine draining the
// deployment's queue into it.
type localWorker struct {
	node   node.Node
	cancel context.CancelFunc
	done   chan struct{}
}

var (
	_ ControlPlane = (*Memory)(nil)
	_ Consumer     = (*Memory)(nil)
)

// NewMemory returns an empty control plane that creates nodes with factory
// and records metrics to rec (use metrics.Nop() to disable).
func NewMemory(factory node.Factory, rec metrics.Recorder) *Memory {
	if rec == nil {
		rec = metrics.Nop()
	}
	return &Memory{
		factory:     factory,
		rec:         rec,
		deployments: make(map[string]*deployment),
	}
}

// Deploy implements ControlPlane.
func (m *Memory) Deploy(ctx context.Context, spec ServiceSpec) error {
	if spec.Name == "" {
		return fmt.Errorf("controlplane: deployment name required")
	}
	pdp, err := spec.PDP()
	if err != nil {
		return err
	}
	replicas := spec.Replicas
	switch {
	case replicas == 0:
		replicas = 1
	case replicas < 0:
		replicas = 0 // remote consumers only
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.deployments[spec.Name]; exists {
		return fmt.Errorf("%w: %q", ErrExists, spec.Name)
	}
	d := &deployment{
		spec:    spec,
		pdp:     pdp,
		queue:   queue.NewMemory(0),
		results: queue.NewMemoryResults(),
	}
	if err := m.resize(ctx, d, replicas); err != nil {
		return err
	}
	m.deployments[spec.Name] = d
	return nil
}

// Scale implements ControlPlane.
func (m *Memory) Scale(ctx context.Context, name string, replicas int) error {
	if replicas < 0 {
		return fmt.Errorf("controlplane: replicas must be >= 0, got %d", replicas)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deployments[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return m.resize(ctx, d, replicas)
}

// resize grows or shrinks d's local consumers to want. Callers must hold m.mu.
func (m *Memory) resize(ctx context.Context, d *deployment, want int) error {
	name := d.spec.Name
	for len(d.workers) < want {
		n, err := m.factory.New(ctx, node.Spec{
			Deployment: name,
			PDP:        d.pdp,
			Spawn: func(ctx context.Context, t task.Task) (string, error) {
				return m.SubmitAsync(ctx, name, t)
			},
		})
		if err != nil {
			return fmt.Errorf("controlplane: scale %q: %w", name, err)
		}
		wctx, cancel := context.WithCancel(context.Background())
		lw := &localWorker{node: n, cancel: cancel, done: make(chan struct{})}
		w := &worker.Worker{
			Deployment: name,
			Queue:      d.queue,
			Results:    d.results,
			Node:       n,
			Recorder:   m.rec,
		}
		go func() {
			defer close(lw.done)
			w.Run(wctx) //nolint:errcheck // only returns ctx.Err on shutdown
		}()
		d.workers = append(d.workers, lw)
	}
	for len(d.workers) > want {
		lw := d.workers[len(d.workers)-1]
		lw.cancel()
		<-lw.done
		if err := lw.node.Close(ctx); err != nil {
			return fmt.Errorf("controlplane: scale %q: close node %s: %w", name, lw.node.ID(), err)
		}
		d.workers = d.workers[:len(d.workers)-1]
	}
	m.rec.Gauge("playscape_nodes", float64(len(d.workers)),
		metrics.Label{Key: "deployment", Value: name})
	return nil
}

// SubmitAsync implements ControlPlane.
func (m *Memory) SubmitAsync(ctx context.Context, name string, t task.Task) (string, error) {
	m.mu.Lock()
	d, ok := m.deployments[name]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if t.Tool == "" {
		return "", fmt.Errorf("controlplane: task tool required")
	}
	if t.ID == "" {
		t.ID = task.NewID()
	}
	if err := d.queue.Enqueue(ctx, t); err != nil {
		return "", fmt.Errorf("controlplane: submit to %q: %w", name, err)
	}
	m.rec.Count("playscape_tasks_submitted_total", 1,
		metrics.Label{Key: "deployment", Value: name},
		metrics.Label{Key: "tool", Value: t.Tool})
	return t.ID, nil
}

// Submit implements ControlPlane.
func (m *Memory) Submit(ctx context.Context, name string, t task.Task) (task.Result, error) {
	m.mu.Lock()
	d, ok := m.deployments[name]
	m.mu.Unlock()
	if !ok {
		return task.Result{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	id, err := m.SubmitAsync(ctx, name, t)
	if err != nil {
		return task.Result{}, err
	}
	res, err := d.results.Await(ctx, id)
	if err != nil {
		return task.Result{}, fmt.Errorf("controlplane: await %q task %s: %w", name, id, err)
	}
	if res.Error != "" {
		return res, fmt.Errorf("controlplane: task %s: %s", id, res.Error)
	}
	return res, nil
}

// Result implements ControlPlane.
func (m *Memory) Result(ctx context.Context, name, taskID string) (task.Result, bool, error) {
	m.mu.Lock()
	d, ok := m.deployments[name]
	m.mu.Unlock()
	if !ok {
		return task.Result{}, false, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return d.results.Get(ctx, taskID)
}

// Spec implements ControlPlane.
func (m *Memory) Spec(_ context.Context, name string) (ServiceSpec, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deployments[name]
	if !ok {
		return ServiceSpec{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return d.spec, nil
}

// Lease implements Consumer: remote workers compete on the same queue as
// local ones.
func (m *Memory) Lease(ctx context.Context, name string) (task.Task, error) {
	m.mu.Lock()
	d, ok := m.deployments[name]
	m.mu.Unlock()
	if !ok {
		return task.Task{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return d.queue.Dequeue(ctx)
}

// Report implements Consumer. Task metrics for remote executions are
// recorded here since the remote worker's recorder lives in its own process.
func (m *Memory) Report(ctx context.Context, name string, r task.Result) error {
	m.mu.Lock()
	d, ok := m.deployments[name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	status := "ok"
	if r.Error != "" {
		status = "error"
	}
	m.rec.Count("playscape_tasks_total", 1,
		metrics.Label{Key: "deployment", Value: name},
		metrics.Label{Key: "source", Value: "remote"},
		metrics.Label{Key: "status", Value: status})
	return d.results.Report(ctx, r)
}

// Status implements ControlPlane.
func (m *Memory) Status(ctx context.Context, name string) (DeploymentStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deployments[name]
	if !ok {
		return DeploymentStatus{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return m.status(ctx, d), nil
}

// List implements ControlPlane.
func (m *Memory) List(ctx context.Context) ([]DeploymentStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]DeploymentStatus, 0, len(m.deployments))
	for _, d := range m.deployments {
		out = append(out, m.status(ctx, d))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// status builds a DeploymentStatus. Callers must hold m.mu.
func (m *Memory) status(ctx context.Context, d *deployment) DeploymentStatus {
	nodes := make([]NodeStatus, len(d.workers))
	for i, lw := range d.workers {
		nodes[i] = NodeStatus{ID: lw.node.ID(), Health: string(lw.node.Health(ctx))}
	}
	return DeploymentStatus{Name: d.spec.Name, Replicas: len(d.workers), Nodes: nodes}
}
