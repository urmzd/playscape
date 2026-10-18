// Package controlplane defines the deployment surface of playscape: deploy a
// single service, scale its agent execution nodes, produce tasks for them,
// and observe the result. It is the composition root — the only package that
// knows about queues, nodes, sandbox policies, and metrics together; each of
// those stays ignorant of the others.
//
// Execution is producer/consumer: Submit and SubmitAsync enqueue tasks, and
// competing consumers dequeue them. Scale adjusts the local consumers;
// remote consumers (Kubernetes pods, serverless containers running
// `playscape work`) attach to the same deployment through the Consumer
// interface and scale independently under their own orchestrator.
package controlplane

import (
	"context"
	"errors"

	"github.com/urmzd/playscape/pkg/ngac"
	"github.com/urmzd/playscape/pkg/sandbox"
	"github.com/urmzd/playscape/pkg/task"
)

// ErrNotFound is returned when a deployment name is unknown.
var ErrNotFound = errors.New("controlplane: deployment not found")

// ErrExists is returned when deploying a name that is already taken.
var ErrExists = errors.New("controlplane: deployment already exists")

// ServiceSpec declares one service: a set of tools, the capabilities each is
// granted, and how many local nodes to start with.
type ServiceSpec struct {
	// Name uniquely identifies the deployment.
	Name string `json:"name"`
	// Replicas is the initial local node count. Zero means one; deploy
	// with a negative value for no local nodes (remote consumers only).
	Replicas int `json:"replicas,omitempty"`
	// Policies grant each tool its workspace areas and spawn targets. A
	// tool not listed here executes with no workspace access and no
	// ability to spawn (default deny). Compiled into an NGAC graph;
	// mutually exclusive with Access.
	Policies []sandbox.Policy `json:"policies,omitempty"`
	// Access is the full NGAC form: define user attributes grouping
	// agents, object attributes over workspace areas and spawn targets,
	// associations, and prohibitions. Use it when flat per-tool policies
	// cannot express the relationships you need.
	Access *ngac.Spec `json:"access,omitempty"`
}

// PDP compiles the spec's access definition into the decision point that
// governs its nodes. Exactly one of Policies or Access may be set; an empty
// spec yields a deny-all PDP.
func (s ServiceSpec) PDP() (sandbox.PDP, error) {
	if s.Access != nil {
		if len(s.Policies) > 0 {
			return nil, errors.New("controlplane: define access with either policies or access, not both")
		}
		return ngac.Build(*s.Access)
	}
	return sandbox.FromPolicies(s.Policies), nil
}

// NodeStatus reports one local node's identity and condition.
type NodeStatus struct {
	ID     string `json:"id"`
	Health string `json:"health"`
}

// DeploymentStatus reports a deployment's current shape. Replicas counts
// local nodes only; remote consumers are owned and counted by their own
// orchestrator.
type DeploymentStatus struct {
	Name     string       `json:"name"`
	Replicas int          `json:"replicas"`
	Nodes    []NodeStatus `json:"nodes"`
}

// ControlPlane is the producer-facing surface: deploy, scale, submit,
// observe.
type ControlPlane interface {
	// Deploy creates a deployment from spec and starts its local nodes.
	Deploy(ctx context.Context, spec ServiceSpec) error
	// Scale sets the deployment's local node count.
	Scale(ctx context.Context, name string, replicas int) error
	// Submit enqueues t and blocks until its result arrives.
	Submit(ctx context.Context, name string, t task.Task) (task.Result, error)
	// SubmitAsync enqueues t and returns its task ID immediately.
	SubmitAsync(ctx context.Context, name string, t task.Task) (string, error)
	// Result returns the result for a task if it has arrived.
	Result(ctx context.Context, name, taskID string) (task.Result, bool, error)
	// Spec returns the deployment's service spec (remote consumers fetch
	// it to reconstruct sandbox policies).
	Spec(ctx context.Context, name string) (ServiceSpec, error)
	// Status reports one deployment.
	Status(ctx context.Context, name string) (DeploymentStatus, error)
	// List reports every deployment in lexical name order.
	List(ctx context.Context) ([]DeploymentStatus, error)
}

// Consumer is the consumer-facing surface remote workers attach to: lease
// work, report outcomes.
type Consumer interface {
	// Lease blocks until a task is available for the deployment or ctx
	// is done.
	Lease(ctx context.Context, name string) (task.Task, error)
	// Report records the result of a leased task.
	Report(ctx context.Context, name string, r task.Result) error
}
