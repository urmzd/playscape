package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/urmzd/playscape/pkg/controlplane"
	"github.com/urmzd/playscape/pkg/metrics"
	"github.com/urmzd/playscape/pkg/ngac"
	"github.com/urmzd/playscape/pkg/node/inproc"
	"github.com/urmzd/playscape/pkg/runner"
	"github.com/urmzd/playscape/pkg/sandbox"
	"github.com/urmzd/playscape/pkg/task"
	"github.com/urmzd/playscape/pkg/tool"
	"github.com/urmzd/playscape/pkg/workspace"
)

const runUsage = `Usage: playscape run [flags] COMMAND

Runs COMMAND through a shell as one playscape task, the way a program
embedding the library would: a local workspace, a tool resolved by an
in-process node, and an NGAC policy deciding every workspace operation.

Before the command starts, every workspace file the policy lets it read is
copied into a scratch directory, which is the command's working directory.
The command's output streams as it runs. When it exits, each file it
created, changed or removed there is applied to the workspace through the
policy, and each change the policy refuses is reported as denied.

The command is a host process with your privileges (runner.Local). The
policy decides what it sees of the workspace and what it may change there,
not what it can reach on this machine.

Exit status: the command's own, or 77 when it succeeded but a change was
denied, 124 on timeout, 125 when it could not run, 130 when interrupted,
2 for bad arguments.

Flags:
`

// maxCapturedOutput caps the output kept for a -json report.
const maxCapturedOutput = 1 << 20

// report is what one run did. It is printed as text, or as JSON with -json.
type report struct {
	Command     string   `json:"command"`
	User        string   `json:"user"`
	ExitCode    int      `json:"exit_code"`
	TimedOut    bool     `json:"timed_out,omitempty"`
	Interrupted bool     `json:"interrupted,omitempty"`
	Duration    string   `json:"duration"`
	Staged      []string `json:"staged,omitempty"`
	Written     []string `json:"written,omitempty"`
	Deleted     []string `json:"deleted,omitempty"`
	Denied      []denial `json:"denied,omitempty"`
	Skipped     []string `json:"skipped,omitempty"`
	Output      string   `json:"output,omitempty"`
	Truncated   bool     `json:"output_truncated,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// denial is one workspace change the policy refused.
type denial struct {
	Op     string `json:"op"`
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// listFlag collects a repeatable string flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// runConfig is the parsed command line of run.
type runConfig struct {
	command string
	user    string
	dir     string
	spec    controlplane.ServiceSpec
	env     map[string]string
	timeout time.Duration
	json    bool
}

func runCommand(args []string, stdout, stderr io.Writer) int {
	cfg, code, ok := parseRun(args, stderr)
	if !ok {
		return code
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rep, err := execRun(ctx, cfg, stdout)
	if err != nil {
		rep.Error = err.Error()
	}
	if cfg.json {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(stderr, "playscape: %v\n", err)
			return exitFailed
		}
	} else {
		printReport(stderr, rep)
	}
	return exitCode(rep)
}

// parseRun parses run's flags. ok is false when the run must not go ahead;
// code is then the exit status.
func parseRun(args []string, stderr io.Writer) (cfg runConfig, code int, ok bool) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, runUsage)
		fs.PrintDefaults()
	}
	var areas, reads, env listFlag
	dir := fs.String("workspace", "", "workspace `directory` (default: a temporary one, removed afterwards)")
	fs.Var(&areas, "area", "grant read and write on a workspace key `prefix` (repeatable)")
	fs.Var(&reads, "read", "grant read only on a workspace key `prefix` (repeatable)")
	policy := fs.String("policy", "", "NGAC policy `file` (a JSON ngac.Spec), in place of -area and -read")
	user := fs.String("as", "run", "the command's `user` in the policy graph")
	fs.Var(&env, "env", "pass `NAME` through from the host, or set NAME=VALUE (repeatable; PATH is passed by default)")
	timeout := fs.Duration("timeout", runner.DefaultTimeout, "how long the command may run")
	asJSON := fs.Bool("json", false, "print the report as JSON on stdout, with the output captured in it")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return cfg, 0, false
		}
		return cfg, exitUsage, false
	}
	usageErr := func(format string, a ...any) (runConfig, int, bool) {
		fmt.Fprintf(stderr, "playscape run: "+format+"\n", a...)
		return cfg, exitUsage, false
	}

	cfg = runConfig{
		command: strings.Join(fs.Args(), " "),
		user:    *user,
		dir:     *dir,
		timeout: *timeout,
		json:    *asJSON,
		env:     map[string]string{},
	}
	if strings.TrimSpace(cfg.command) == "" {
		return usageErr("no command given")
	}
	if cfg.user == "" {
		return usageErr("-as needs a user name")
	}
	if cfg.timeout <= 0 {
		return usageErr("-timeout must be positive")
	}
	if p, ok := os.LookupEnv("PATH"); ok {
		cfg.env["PATH"] = p
	}
	for _, e := range env {
		name, value, set := strings.Cut(e, "=")
		if name == "" {
			return usageErr("-env %q: empty name", e)
		}
		if !set {
			v, ok := os.LookupEnv(name)
			if !ok {
				continue
			}
			value = v
		}
		cfg.env[name] = value
	}

	cfg.spec = controlplane.ServiceSpec{Name: "run", Replicas: 1}
	switch {
	case *policy != "" && len(areas)+len(reads) > 0:
		return usageErr("use -policy or -area/-read, not both")
	case *policy != "":
		spec, err := readPolicy(*policy)
		if err != nil {
			return usageErr("%v", err)
		}
		cfg.spec.Access = &spec
	default:
		p := sandbox.Policy{Tool: cfg.user}
		for _, a := range areas {
			p.Areas = append(p.Areas, sandbox.Area{Prefix: a})
		}
		for _, a := range reads {
			p.Areas = append(p.Areas, sandbox.Area{Prefix: a, ReadOnly: true})
		}
		for _, a := range p.Areas {
			if !workspace.ValidKey(a.Prefix) {
				return usageErr("area %q is not a workspace key prefix (relative, slash-separated, no . or ..)", a.Prefix)
			}
		}
		cfg.spec.Policies = []sandbox.Policy{p}
	}
	return cfg, 0, true
}

// readPolicy reads an NGAC spec, refusing fields it does not know so a typo
// cannot silently grant nothing.
func readPolicy(path string) (ngac.Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ngac.Spec{}, fmt.Errorf("policy: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var spec ngac.Spec
	if err := dec.Decode(&spec); err != nil {
		return ngac.Spec{}, fmt.Errorf("policy %s: %w", path, err)
	}
	return spec, nil
}

// execRun composes the library pieces a program would, runs the command as
// one task, and returns what it did. ctx ending interrupts the command.
func execRun(ctx context.Context, cfg runConfig, stdout io.Writer) (report, error) {
	rep := report{Command: cfg.command, User: cfg.user}

	dir := cfg.dir
	if dir == "" {
		tmp, err := os.MkdirTemp("", "playscape-workspace-*")
		if err != nil {
			return rep, fmt.Errorf("workspace: %w", err)
		}
		defer os.RemoveAll(tmp)
		dir = tmp
	}
	ws, err := workspace.NewLocal(dir)
	if err != nil {
		return rep, err
	}

	var out io.Writer = stdout
	var captured *capture
	if cfg.json {
		captured = &capture{max: maxCapturedOutput}
		out = captured
	}

	registry := tool.NewRegistry()
	err = registry.Register(tool.Func(cfg.user, func(tctx context.Context, rt tool.Runtime, _ []byte) ([]byte, error) {
		// The task runs under the control plane's context; an interrupt
		// of playscape (ctx) ends it too.
		tctx, cancel := context.WithCancel(tctx)
		defer cancel()
		defer context.AfterFunc(ctx, cancel)()
		return nil, stageAndRun(tctx, rt.Workspace(), cfg, out, &rep)
	}))
	if err != nil {
		return rep, err
	}

	plane := controlplane.NewMemory(inproc.NewFactory(registry, ws), metrics.Nop())
	if err := plane.Deploy(ctx, cfg.spec); err != nil {
		return rep, err
	}
	// Wait on the task with a context of its own: an interrupt has to
	// reach the command and its process group, and be reported, before
	// playscape exits. The result arriving orders the tool's writes to
	// rep before the reads below.
	_, err = plane.Submit(context.Background(), cfg.spec.Name, task.Task{Tool: cfg.user})
	if captured != nil {
		rep.Output, rep.Truncated = captured.String(), captured.truncated
	}
	return rep, err
}

// stageAndRun is the tool body: stage what ws lets the user read, run the
// command over it, and apply its changes back through ws. Every workspace
// operation here goes through the policy-scoped view the node handed the
// tool.
func stageAndRun(ctx context.Context, ws workspace.Workspace, cfg runConfig, out io.Writer, rep *report) error {
	scratch, err := os.MkdirTemp("", "playscape-scratch-*")
	if err != nil {
		return fmt.Errorf("scratch: %w", err)
	}
	defer os.RemoveAll(scratch)
	root, err := os.OpenRoot(scratch)
	if err != nil {
		return fmt.Errorf("scratch: %w", err)
	}
	defer root.Close()

	staged, err := stage(ctx, ws, root)
	if err != nil {
		return err
	}
	for k := range staged {
		rep.Staged = append(rep.Staged, k)
	}
	sort.Strings(rep.Staged)

	shell := []string{"/bin/sh", "-c"}
	if runtime.GOOS == "windows" {
		shell = []string{"cmd", "/C"}
	}
	start := time.Now()
	res, err := runner.Local{Dir: scratch}.Run(ctx, runner.Request{
		Command: append(shell, cfg.command),
		Env:     cfg.env,
		Timeout: cfg.timeout,
	}, out)
	rep.Duration = time.Since(start).Round(time.Millisecond).String()
	switch {
	case errors.Is(err, runner.ErrTimeout):
		rep.TimedOut = true
		return nil
	case err != nil && ctx.Err() != nil:
		rep.Interrupted = true
		return nil
	case err != nil:
		return err
	}
	rep.ExitCode = res.ExitCode
	return apply(context.WithoutCancel(ctx), ws, root, staged, rep)
}

// stage copies every key ws lets the user read into root and returns the
// digest of each, so apply can tell what the command changed.
func stage(ctx context.Context, ws workspace.Workspace, root *os.Root) (map[string][sha256.Size]byte, error) {
	keys, err := ws.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("stage: %w", err)
	}
	staged := make(map[string][sha256.Size]byte, len(keys))
	for _, key := range keys {
		rc, err := ws.Read(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("stage %s: %w", key, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("stage %s: %w", key, err)
		}
		name := filepath.FromSlash(key)
		if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return nil, fmt.Errorf("stage %s: %w", key, err)
		}
		if err := root.WriteFile(name, data, 0o644); err != nil {
			return nil, fmt.Errorf("stage %s: %w", key, err)
		}
		staged[key] = sha256.Sum256(data)
	}
	return staged, nil
}

// apply writes each file the command created or changed in root back to ws,
// and deletes each staged key the command removed. A change the policy
// refuses is recorded as denied; anything that is not a regular file (a
// symlink, a device) is skipped, never followed.
func apply(ctx context.Context, ws workspace.Workspace, root *os.Root, staged map[string][sha256.Size]byte, rep *report) error {
	seen := make(map[string]bool)
	err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		key := name // fs.FS paths are already slash-separated
		if !d.Type().IsRegular() || !workspace.ValidKey(key) {
			rep.Skipped = append(rep.Skipped, key)
			return nil
		}
		seen[key] = true
		data, err := root.ReadFile(filepath.FromSlash(name))
		if err != nil {
			return err
		}
		if sum, ok := staged[key]; ok && sum == sha256.Sum256(data) {
			return nil
		}
		return record(rep, "write", key, &rep.Written, ws.Write(ctx, key, bytes.NewReader(data)))
	})
	if err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	removed := make([]string, 0)
	for key := range staged {
		if !seen[key] {
			removed = append(removed, key)
		}
	}
	sort.Strings(removed)
	for _, key := range removed {
		if err := record(rep, "delete", key, &rep.Deleted, ws.Delete(ctx, key)); err != nil {
			return fmt.Errorf("apply: %w", err)
		}
	}
	return nil
}

// record files the outcome of one workspace change: done, denied by the
// policy, or a failure that ends the run.
func record(rep *report, op, key string, done *[]string, err error) error {
	switch {
	case err == nil:
		*done = append(*done, key)
		return nil
	case errors.Is(err, sandbox.ErrDenied):
		rep.Denied = append(rep.Denied, denial{Op: op, Key: key, Reason: err.Error()})
		return nil
	default:
		return fmt.Errorf("%s %s: %w", op, key, err)
	}
}

// printReport writes rep for a person.
func printReport(w io.Writer, rep report) {
	if rep.Duration != "" {
		fmt.Fprintf(w, "playscape: ran as %q for %s\n", rep.User, rep.Duration)
	}
	fmt.Fprintf(w, "playscape: staged %d file(s) the policy lets %q read\n", len(rep.Staged), rep.User)
	for _, k := range rep.Written {
		fmt.Fprintf(w, "playscape: wrote %s\n", k)
	}
	for _, k := range rep.Deleted {
		fmt.Fprintf(w, "playscape: deleted %s\n", k)
	}
	for _, d := range rep.Denied {
		fmt.Fprintf(w, "playscape: DENIED %s %s (%s)\n", d.Op, d.Key, d.Reason)
	}
	for _, k := range rep.Skipped {
		fmt.Fprintf(w, "playscape: skipped %s (not a regular file, or not a valid workspace key)\n", k)
	}
	switch {
	case rep.Error != "":
		fmt.Fprintf(w, "playscape: FAILED: %s\n", rep.Error)
	case rep.Interrupted:
		fmt.Fprintln(w, "playscape: INTERRUPTED; nothing was applied to the workspace")
	case rep.TimedOut:
		fmt.Fprintf(w, "playscape: TIMED OUT after %s; nothing was applied to the workspace\n", rep.Duration)
	case rep.ExitCode != 0:
		fmt.Fprintf(w, "playscape: command FAILED with exit code %d\n", rep.ExitCode)
	case len(rep.Denied) > 0:
		fmt.Fprintf(w, "playscape: command exited 0, but the policy DENIED %d change(s)\n", len(rep.Denied))
	default:
		fmt.Fprintln(w, "playscape: ok")
	}
}

// exitCode maps a report to playscape's exit status.
func exitCode(rep report) int {
	switch {
	case rep.Error != "":
		return exitFailed
	case rep.Interrupted:
		return exitInterrupted
	case rep.TimedOut:
		return exitTimeout
	case rep.ExitCode != 0:
		return rep.ExitCode
	case len(rep.Denied) > 0:
		return exitDenied
	default:
		return 0
	}
}

// capture keeps the first max bytes written and drops the rest. Writes come
// from the runner's serialized log stream.
type capture struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *capture) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room < len(p) {
		c.truncated = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *capture) String() string { return c.buf.String() }
