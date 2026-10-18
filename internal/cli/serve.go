package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/urmzd/playscape/internal/server"
	"github.com/urmzd/playscape/pkg/controlplane"
	"github.com/urmzd/playscape/pkg/metrics"
	"github.com/urmzd/playscape/pkg/node/inproc"
	"github.com/urmzd/playscape/pkg/tool"
	"github.com/urmzd/playscape/pkg/workspace"
)

func newServeCmd() *cobra.Command {
	var (
		addr string
		root string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the control plane server",
		Long: "Run the beta control plane: an HTTP API producing tasks onto per-deployment\n" +
			"queues, consumed by local worker goroutines and by remote `playscape work`\n" +
			"replicas (Kubernetes pods, serverless containers). A built-in \"echo\" tool is\n" +
			"registered (sandboxed to the \"echo/\" area) so the API is exercisable out of\n" +
			"the box; real deployments register their own tools via the Go SDK.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ws, err := workspace.NewLocal(root)
			if err != nil {
				return err
			}

			registry := tool.NewRegistry()
			if err := registry.Register(echoTool()); err != nil {
				return err
			}

			rec := metrics.NewMemory()
			plane := controlplane.NewMemory(inproc.NewFactory(registry, ws), rec)

			srv := &http.Server{
				Addr:              addr,
				Handler:           server.New(plane, plane, rec),
				ReadHeaderTimeout: 10 * time.Second,
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			errc := make(chan error, 1)
			go func() { errc <- srv.ListenAndServe() }()
			fmt.Fprintf(os.Stderr, "playscape (beta) listening on %s, workspace at %s\n", addr, root)

			select {
			case err := <-errc:
				return err
			case <-ctx.Done():
			}
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", ":8484", "Listen address")
	cmd.Flags().StringVar(&root, "workspace", ".playscape/workspace", "Workspace root directory")
	return cmd
}

// echoTool writes its input to the workspace area it is sandboxed to and
// echoes it back — the smallest possible demonstration of a scoped tool.
func echoTool() tool.Tool {
	return tool.Func("echo", func(ctx context.Context, rt tool.Runtime, input []byte) ([]byte, error) {
		if err := rt.Workspace().Write(ctx, "echo/last", bytes.NewReader(input)); err != nil {
			return nil, err
		}
		return input, nil
	})
}
