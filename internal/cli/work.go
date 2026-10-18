package cli

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/urmzd/playscape/pkg/controlplane"
	"github.com/urmzd/playscape/pkg/metrics"
	"github.com/urmzd/playscape/pkg/node"
	"github.com/urmzd/playscape/pkg/node/inproc"
	"github.com/urmzd/playscape/pkg/queue/httpqueue"
	"github.com/urmzd/playscape/pkg/tool"
	"github.com/urmzd/playscape/pkg/worker"
	"github.com/urmzd/playscape/pkg/workspace"
)

func newWorkCmd() *cobra.Command {
	var (
		serverURL   string
		deployment  string
		concurrency int
		root        string
	)
	cmd := &cobra.Command{
		Use:   "work",
		Short: "Run a remote consumer for a deployment",
		Long: "Run the consumer loop against a control plane: lease tasks from the\n" +
			"deployment's queue, execute them on local nodes, and report results.\n" +
			"This is the process Kubernetes deployments and serverless containers\n" +
			"replicate to scale execution out — every replica competes on the same\n" +
			"queue. Sandbox policies are fetched from the deployment's spec.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if deployment == "" {
				return fmt.Errorf("--deployment is required")
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			client := httpqueue.New(serverURL, deployment)
			// Poll for the spec: in Kubernetes, worker pods may start
			// before the deployment has been created on the plane.
			var (
				spec controlplane.ServiceSpec
				err  error
			)
			for {
				spec, err = client.Spec(ctx)
				if err == nil {
					break
				}
				fmt.Fprintf(os.Stderr, "waiting for deployment %q: %v\n", deployment, err)
				select {
				case <-time.After(2 * time.Second):
				case <-ctx.Done():
					return ctx.Err()
				}
			}

			pdp, err := spec.PDP()
			if err != nil {
				return fmt.Errorf("compile access definition: %w", err)
			}

			ws, err := workspace.NewLocal(root)
			if err != nil {
				return err
			}
			registry := tool.NewRegistry()
			if err := registry.Register(echoTool()); err != nil {
				return err
			}
			factory := inproc.NewFactory(registry, ws)

			rec := metrics.NewMemory()
			var wg sync.WaitGroup
			for i := 0; i < concurrency; i++ {
				n, err := factory.New(ctx, node.Spec{
					Deployment: deployment,
					PDP:        pdp,
					Spawn:      client.SubmitAsync,
				})
				if err != nil {
					return err
				}
				w := &worker.Worker{
					Deployment: deployment,
					Queue:      client,
					Results:    client,
					Node:       n,
					Recorder:   rec,
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					w.Run(ctx) //nolint:errcheck // only returns ctx.Err on shutdown
				}()
			}
			fmt.Fprintf(os.Stderr, "playscape worker (beta): %d consumers on %q against %s\n",
				concurrency, deployment, serverURL)

			wg.Wait() // workers exit when ctx is canceled by a signal
			return nil
		},
	}
	cmd.Flags().StringVar(&serverURL, "server", "http://localhost:8484", "Control plane base URL")
	cmd.Flags().StringVar(&deployment, "deployment", "", "Deployment to consume tasks for")
	cmd.Flags().IntVar(&concurrency, "concurrency", 2, "Concurrent consumers in this process")
	cmd.Flags().StringVar(&root, "workspace", ".playscape/workspace", "Workspace root directory")
	return cmd
}
