// Saige integration example: run saige agents as playscape workloads.
//
// Each playscape task runs a saige agent inside a sandboxed tool. The agent's
// answer is written to the tool's workspace area ("agents/..."); a write
// outside that area is blocked by the sandbox. The deployment scales to
// three nodes, and the agent is self-referential: when asked to delegate,
// it spawns a sub-task that another node picks up (an agent calling an
// agent), gated by the policy's spawn allowlist.
//
// The agent uses saige's agenttest.ScriptedProvider, so no LLM backend or
// API key is required. Swap in an Ollama/OpenAI/Anthropic provider for real
// inference.
//
// Prerequisites: none. Run with:
//
//	go run ./examples/saige/
package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	saige "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"

	"github.com/urmzd/playscape/pkg/controlplane"
	"github.com/urmzd/playscape/pkg/metrics"
	"github.com/urmzd/playscape/pkg/node/inproc"
	"github.com/urmzd/playscape/pkg/sandbox"
	"github.com/urmzd/playscape/pkg/task"
	"github.com/urmzd/playscape/pkg/tool"
	"github.com/urmzd/playscape/pkg/workspace"
)

const delegatePrefix = "delegate:"

// agentTool wraps a saige agent as a playscape tool. Each call constructs a
// fresh agent (its own conversation tree), invokes it with the task input,
// and persists the answer to the tool's sandboxed workspace area. Inputs
// beginning with "delegate:" make the agent spawn a sub-task for the rest —
// the self-referential path.
func agentTool() tool.Tool {
	return tool.Func("saige-agent", func(ctx context.Context, rt tool.Runtime, input []byte) ([]byte, error) {
		if rest, ok := strings.CutPrefix(string(input), delegatePrefix); ok {
			id, err := rt.Spawn(ctx, task.Task{Tool: "saige-agent", Input: []byte(rest)})
			if err != nil {
				return nil, err
			}
			return []byte("delegated as task " + id), nil
		}

		provider := &agenttest.ScriptedProvider{
			Responses: [][]types.Delta{
				agenttest.TextResponse(fmt.Sprintf("considered %q and dispatched accordingly", input)),
			},
		}
		ag, err := saige.New(saige.Config{
			Name:         "worker",
			SystemPrompt: "You are a playscape worker agent.",
			Provider:     provider,
		})

		if err != nil {
			return nil, fmt.Errorf("create saige agent: %w", err)
		}

		stream := ag.Invoke(ctx, []types.Message{types.UserMsg(types.Text(string(input)))})
		answer := agenttest.CollectText(stream.Deltas())
		if err := stream.Wait(); err != nil {
			return nil, fmt.Errorf("saige agent: %w", err)
		}

		// Allowed: "agents/..." is this tool's declared area.
		key := fmt.Sprintf("agents/answers/%x", input)
		if err := rt.Workspace().Write(ctx, key, bytes.NewReader([]byte(answer))); err != nil {
			return nil, err
		}
		// Denied: outside the area — the sandbox stops the leak.
		if err := rt.Workspace().Write(ctx, "secrets/exfil", bytes.NewReader([]byte(answer))); err != nil {
			fmt.Printf("sandbox blocked the leak: %v\n", err)
		}
		return []byte(answer), nil
	})
}

func main() {
	ctx := context.Background()

	dir, err := os.MkdirTemp("", "playscape-saige-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ws, err := workspace.NewLocal(dir)
	if err != nil {
		log.Fatal(err)
	}

	registry := tool.NewRegistry()
	if err := registry.Register(agentTool()); err != nil {
		log.Fatal(err)
	}

	rec := metrics.NewMemory()
	plane := controlplane.NewMemory(inproc.NewFactory(registry, ws), rec)

	err = plane.Deploy(ctx, controlplane.ServiceSpec{
		Name:     "saige-workers",
		Replicas: 1,
		Policies: []sandbox.Policy{{
			Tool:  "saige-agent",
			Areas: []sandbox.Area{{Prefix: "agents"}},
			Spawn: []string{"saige-agent"}, // agents may call agents
		}},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := plane.Scale(ctx, "saige-workers", 3); err != nil {
		log.Fatal(err)
	}

	for _, prompt := range []string{"triage inbox", "summarize logs"} {
		res, err := plane.Submit(ctx, "saige-workers", task.Task{Tool: "saige-agent", Input: []byte(prompt)})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s → %s: %s\n", prompt, res.NodeID, res.Output)
	}

	// Self-referential: the agent spawns a sub-agent for the delegated work.
	res, err := plane.Submit(ctx, "saige-workers", task.Task{
		Tool:  "saige-agent",
		Input: []byte(delegatePrefix + "plan release"),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("delegate → %s: %s\n", res.NodeID, res.Output)

	childID := strings.TrimPrefix(string(res.Output), "delegated as task ")
	child, err := awaitResult(ctx, plane, "saige-workers", childID)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("  child %s → %s: %s\n", childID[:8], child.NodeID, child.Output)

	keys, err := ws.List(ctx, "agents")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nworkspace now holds %d agent artifacts under agents/\n", len(keys))

	fmt.Println("\nmetrics:")
	snap := rec.Snapshot()
	names := make([]string, 0, len(snap))
	for k := range snap {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Printf("  %s %g\n", k, snap[k])
	}
}

// awaitResult polls for an async task's result.
func awaitResult(ctx context.Context, plane *controlplane.Memory, name, id string) (task.Result, error) {
	for {
		res, done, err := plane.Result(ctx, name, id)
		if err != nil {
			return task.Result{}, err
		}
		if done {
			if res.Error != "" {
				return res, fmt.Errorf("task %s: %s", id, res.Error)
			}
			return res, nil
		}
	}
}
