// NGAC example: define access as a policy graph instead of flat per-tool
// policies. Two agents are grouped under one user attribute that grants
// read over the "reports" area; the analyst alone may write; a prohibition
// keeps the auditor read-only even though broader grants exist; and the
// analyst may spawn the auditor (an agent calling an agent) through a
// declared spawn object.
//
// Prerequisites: none. Run with:
//
//	go run ./examples/ngac/
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/urmzd/playscape/pkg/controlplane"
	"github.com/urmzd/playscape/pkg/metrics"
	"github.com/urmzd/playscape/pkg/ngac"
	"github.com/urmzd/playscape/pkg/node/inproc"
	"github.com/urmzd/playscape/pkg/sandbox"
	"github.com/urmzd/playscape/pkg/task"
	"github.com/urmzd/playscape/pkg/tool"
	"github.com/urmzd/playscape/pkg/workspace"
)

// access is the NGAC definition: who (users, grouped by attributes) may do
// what (ops) to which (object attributes over workspace areas and spawn
// targets), under one policy class, with a prohibition carving out an
// exception.
var access = &ngac.Spec{
	PolicyClasses: []string{"pc"},
	UserAttrs:     []ngac.Node{{Name: "agents"}},
	Users: []ngac.Node{
		{Name: "analyst", Parents: []string{"agents"}},
		{Name: "auditor", Parents: []string{"agents"}},
	},
	ObjectAttrs: []ngac.ObjectAttr{
		{Name: "reports", Prefix: "reports", Parents: []string{"pc"}},
		{Name: "spawnable", Parents: []string{"pc"}},
	},
	Objects: []ngac.Node{
		{Name: sandbox.SpawnObject("auditor"), Parents: []string{"spawnable"}},
	},
	Associations: []ngac.Association{
		// Everyone in "agents" may read and write reports...
		{UserAttr: "agents", Ops: []string{sandbox.OpRead, sandbox.OpWrite}, Target: "reports"},
		// ...and the analyst may spawn the auditor.
		{UserAttr: "analyst", Ops: []string{sandbox.OpSpawn}, Target: "spawnable"},
	},
	Prohibitions: []ngac.Association{
		// ...but the auditor is read-only, overriding the group grant.
		{UserAttr: "auditor", Ops: []string{sandbox.OpWrite}, Target: "reports"},
	},
}

func analystTool() tool.Tool {
	return tool.Func("analyst", func(ctx context.Context, rt tool.Runtime, in []byte) ([]byte, error) {
		key := "reports/" + string(in)
		if err := rt.Workspace().Write(ctx, key, bytes.NewReader([]byte("findings for "+string(in)))); err != nil {
			return nil, err
		}
		// Outside every granted attribute: denied.
		if err := rt.Workspace().Write(ctx, "private/copy", bytes.NewReader(in)); err != nil {
			fmt.Printf("analyst blocked: %v\n", err)
		}
		id, err := rt.Spawn(ctx, task.Task{Tool: "auditor", Input: []byte(key)})
		if err != nil {
			return nil, err
		}
		return []byte(id), nil
	})
}

func auditorTool() tool.Tool {
	return tool.Func("auditor", func(ctx context.Context, rt tool.Runtime, in []byte) ([]byte, error) {
		rc, err := rt.Workspace().Read(ctx, string(in))
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		content, err := io.ReadAll(rc)
		if err != nil {
			return nil, err
		}
		// The prohibition keeps the auditor read-only in "reports".
		if err := rt.Workspace().Write(ctx, string(in), bytes.NewReader([]byte("tampered"))); err != nil {
			fmt.Printf("auditor blocked: %v\n", err)
		}
		return []byte("audited: " + string(content)), nil
	})
}

func main() {
	ctx := context.Background()

	dir, err := os.MkdirTemp("", "playscape-ngac-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ws, err := workspace.NewLocal(dir)
	if err != nil {
		log.Fatal(err)
	}

	registry := tool.NewRegistry()
	for _, tl := range []tool.Tool{analystTool(), auditorTool()} {
		if err := registry.Register(tl); err != nil {
			log.Fatal(err)
		}
	}

	plane := controlplane.NewMemory(inproc.NewFactory(registry, ws), metrics.Nop())
	err = plane.Deploy(ctx, controlplane.ServiceSpec{
		Name:     "review",
		Replicas: 2,
		Access:   access,
	})
	if err != nil {
		log.Fatal(err)
	}

	res, err := plane.Submit(ctx, "review", task.Task{Tool: "analyst", Input: []byte("q2-incident")})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("analyst on %s spawned audit task %s\n", res.NodeID, res.Output)

	audit := await(ctx, plane, "review", string(res.Output))
	fmt.Printf("auditor on %s: %s\n", audit.NodeID, audit.Output)
}

func await(ctx context.Context, plane *controlplane.Memory, name, id string) task.Result {
	for {
		res, done, err := plane.Result(ctx, name, id)
		if err != nil {
			log.Fatal(err)
		}
		if done {
			if res.Error != "" {
				log.Fatalf("task %s: %s", id, res.Error)
			}
			return res
		}
	}
}
