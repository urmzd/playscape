// Package queue is the producer/consumer seam of playscape. Producers (the
// control plane API, or agents spawning sub-tasks) enqueue tasks; consumers
// (worker processes — goroutines locally, pods or serverless containers at
// scale) compete to dequeue and execute them. Scaling out is nothing more
// than adding consumers, which is why this interface is what Kubernetes-like
// substrates bind to.
//
// The in-memory implementation here serves the single-binary beta. Remote
// consumers use the HTTP-backed implementation in package httpqueue; durable
// brokers (Redis, Pub/Sub, SQS) slot in behind the same two interfaces.
package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/urmzd/playscape/pkg/task"
)

// ErrFull is returned by Enqueue when the queue cannot accept more tasks.
var ErrFull = errors.New("queue: full")

// Queue carries tasks from producers to competing consumers. Delivery is
// at-most-once in the beta: a consumer that dies mid-task drops it.
type Queue interface {
	// Enqueue adds t to the queue.
	Enqueue(ctx context.Context, t task.Task) error
	// Dequeue blocks until a task is available or ctx is done.
	Dequeue(ctx context.Context) (task.Task, error)
}

// Results carries task outcomes back to whoever is waiting on them.
type Results interface {
	// Report records the result of a completed task.
	Report(ctx context.Context, r task.Result) error
	// Await blocks until the result for taskID arrives or ctx is done.
	Await(ctx context.Context, taskID string) (task.Result, error)
	// Get returns the result for taskID if it has arrived.
	Get(ctx context.Context, taskID string) (task.Result, bool, error)
}

// Memory is an in-process Queue for the single-binary deployment.
type Memory struct {
	ch chan task.Task
}

// NewMemory returns a queue buffering up to capacity tasks (default 1024).
func NewMemory(capacity int) *Memory {
	if capacity <= 0 {
		capacity = 1024
	}
	return &Memory{ch: make(chan task.Task, capacity)}
}

// Enqueue implements Queue. It fails fast with ErrFull rather than blocking
// producers behind slow consumers.
func (m *Memory) Enqueue(_ context.Context, t task.Task) error {
	select {
	case m.ch <- t:
		return nil
	default:
		return fmt.Errorf("%w (capacity %d)", ErrFull, cap(m.ch))
	}
}

// Dequeue implements Queue.
func (m *Memory) Dequeue(ctx context.Context) (task.Task, error) {
	select {
	case t := <-m.ch:
		return t, nil
	case <-ctx.Done():
		return task.Task{}, ctx.Err()
	}
}

// MemoryResults is an in-process Results store.
type MemoryResults struct {
	mu      sync.Mutex
	done    map[string]task.Result
	waiters map[string][]chan task.Result
}

// NewMemoryResults returns an empty results store.
func NewMemoryResults() *MemoryResults {
	return &MemoryResults{
		done:    make(map[string]task.Result),
		waiters: make(map[string][]chan task.Result),
	}
}

// Report implements Results.
func (m *MemoryResults) Report(_ context.Context, r task.Result) error {
	m.mu.Lock()
	m.done[r.TaskID] = r
	waiters := m.waiters[r.TaskID]
	delete(m.waiters, r.TaskID)
	m.mu.Unlock()
	for _, w := range waiters {
		w <- r
	}
	return nil
}

// Await implements Results.
func (m *MemoryResults) Await(ctx context.Context, taskID string) (task.Result, error) {
	m.mu.Lock()
	if r, ok := m.done[taskID]; ok {
		m.mu.Unlock()
		return r, nil
	}
	ch := make(chan task.Result, 1)
	m.waiters[taskID] = append(m.waiters[taskID], ch)
	m.mu.Unlock()

	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return task.Result{}, ctx.Err()
	}
}

// Get implements Results.
func (m *MemoryResults) Get(_ context.Context, taskID string) (task.Result, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.done[taskID]
	return r, ok, nil
}
