// Package tool defines the unit of capability an agent execution node can
// invoke. A tool is a function over bytes plus a Runtime; it knows nothing
// about sandboxing, scheduling, or metrics. The control plane composes those
// concerns around it: the workspace view a tool receives is already scoped
// to its policy's areas, and the tasks it spawns are gated by its policy's
// spawn allowlist.
package tool

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/urmzd/playscape/pkg/task"
	"github.com/urmzd/playscape/pkg/workspace"
)

// Runtime is the capability surface a tool executes against. Both methods
// are confined by the tool's sandbox policy before the tool ever sees them.
type Runtime interface {
	// Workspace returns the tool's sandboxed view of the shared workspace.
	Workspace() workspace.Workspace
	// Spawn submits a sub-task back into the tool's own deployment and
	// returns its task ID without waiting for the result. This is how an
	// agent calls another agent: the child task competes for a node like
	// any other work. The policy's spawn allowlist gates which tools may
	// be targeted; the default is none.
	Spawn(ctx context.Context, t task.Task) (string, error)
}

// Tool is one named capability.
type Tool interface {
	// Name uniquely identifies the tool within a registry and is the key
	// its sandbox policy binds to.
	Name() string
	// Call executes the tool with input against rt.
	Call(ctx context.Context, rt Runtime, input []byte) ([]byte, error)
}

// Func adapts a plain function into a Tool.
func Func(name string, fn func(ctx context.Context, rt Runtime, input []byte) ([]byte, error)) Tool {
	return funcTool{name: name, fn: fn}
}

type funcTool struct {
	name string
	fn   func(context.Context, Runtime, []byte) ([]byte, error)
}

func (t funcTool) Name() string { return t.name }

func (t funcTool) Call(ctx context.Context, rt Runtime, input []byte) ([]byte, error) {
	return t.fn(ctx, rt, input)
}

// Registry holds the tools a deployment may dispatch to. It is safe for
// concurrent use.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds t to the registry. Registering a duplicate name is an error
// so a tool cannot silently shadow another.
func (r *Registry) Register(t Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[t.Name()]; exists {
		return fmt.Errorf("tool: %q already registered", t.Name())
	}
	r.tools[t.Name()] = t
	return nil
}

// Get returns the tool registered under name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names returns all registered tool names in lexical order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
