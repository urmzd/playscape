// Package cli is the "cli" tool provider: each tool wraps one CLI binary
// behind a fixed argv template, an environment allowlist, a timeout, and
// output caps. The agent supplies only template values; it never chooses
// the binary, adds arguments, or reaches a shell.
//
// Orthogonality boundary: this package builds and runs argv. It is not an
// isolation boundary. The binary runs on the worker host with the worker's
// privileges, and the workspace policy that confines a tool's
// tool.Runtime does not confine what the binary itself touches. Run
// untrusted binaries through an isolating executor instead.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/urmzd/playscape/pkg/tool"
)

const (
	// DefaultTimeout bounds a call when Spec.Timeout is zero.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxOutputBytes caps each of stdout and stderr when
	// Spec.MaxOutputBytes is zero.
	DefaultMaxOutputBytes = 1 << 20
	// waitDelay bounds how long a call waits for output pipes after the
	// process exits or is killed (a grandchild may still hold them).
	waitDelay = 2 * time.Second
)

var (
	// ErrInvalidSpec is returned by Provider.Tools for a malformed Spec.
	ErrInvalidSpec = errors.New("cli: invalid spec")
	// ErrInvalidInput is returned when call input does not satisfy the
	// argv template.
	ErrInvalidInput = errors.New("cli: invalid input")
	// ErrTimeout is returned when a call exceeds its timeout.
	ErrTimeout = errors.New("cli: timeout")
)

// placeholder matches one template variable, e.g. "{path}" or
// "--name={name}". Braces around anything that is not an identifier are
// literal text.
var placeholder = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Spec declares one CLI-backed tool.
type Spec struct {
	// Name is the registered tool name and the sandbox policy key.
	Name string
	// Binary is the executable: an absolute path, or a bare name resolved
	// on PATH once, when the provider builds its tools.
	Binary string
	// Args is the argv template (excluding argv[0]). Each element becomes
	// exactly one argument; "{name}" placeholders are replaced with the
	// matching input value. Values are never word-split or shell-expanded.
	Args []string
	// Env lists host environment variable names passed through to the
	// binary. Everything else in the host environment is withheld,
	// including PATH unless listed.
	Env []string
	// SetEnv sets fixed environment variables; it overrides Env.
	SetEnv map[string]string
	// Dir is the working directory. Empty runs each call in a fresh
	// temporary directory removed afterwards.
	Dir string
	// Timeout bounds each call; zero means DefaultTimeout.
	Timeout time.Duration
	// MaxOutputBytes caps each of stdout and stderr; output beyond it is
	// dropped and flagged as truncated. Zero means DefaultMaxOutputBytes.
	MaxOutputBytes int
	// AllowFlagValues permits substituted values that start with "-".
	// It is off by default so an input value cannot smuggle in a flag.
	AllowFlagValues bool
}

// Output is the JSON a CLI tool returns for every run that started and
// exited, whatever its exit code: a non-zero exit is data for the agent,
// not a tool failure.
type Output struct {
	ExitCode        int    `json:"exit_code"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
}

// Provider builds CLI-backed tools from specs.
type Provider struct {
	specs []Spec
}

// New returns a provider for specs.
func New(specs ...Spec) *Provider { return &Provider{specs: specs} }

// Kind implements tool.Provider.
func (p *Provider) Kind() tool.Kind { return tool.KindCLI }

// Tools implements tool.Provider. It validates every spec and resolves
// each binary to an absolute path.
func (p *Provider) Tools() ([]tool.Tool, error) {
	tools := make([]tool.Tool, 0, len(p.specs))
	for _, s := range p.specs {
		t, err := build(s)
		if err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}
	return tools, nil
}

func build(s Spec) (*cliTool, error) {
	if s.Name == "" {
		return nil, fmt.Errorf("%w: empty name", ErrInvalidSpec)
	}
	if s.Binary == "" {
		return nil, fmt.Errorf("%w: %q: empty binary", ErrInvalidSpec, s.Name)
	}
	if s.Timeout < 0 || s.MaxOutputBytes < 0 {
		return nil, fmt.Errorf("%w: %q: negative timeout or output cap", ErrInvalidSpec, s.Name)
	}
	for _, name := range s.Env {
		if name == "" || strings.ContainsRune(name, '=') {
			return nil, fmt.Errorf("%w: %q: bad env name %q", ErrInvalidSpec, s.Name, name)
		}
	}
	bin, err := exec.LookPath(s.Binary)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w", ErrInvalidSpec, s.Name, err)
	}
	if bin, err = filepath.Abs(bin); err != nil {
		return nil, fmt.Errorf("%w: %q: %w", ErrInvalidSpec, s.Name, err)
	}
	if s.Timeout == 0 {
		s.Timeout = DefaultTimeout
	}
	if s.MaxOutputBytes == 0 {
		s.MaxOutputBytes = DefaultMaxOutputBytes
	}
	return &cliTool{spec: s, bin: bin}, nil
}

type cliTool struct {
	spec Spec
	bin  string
}

func (t *cliTool) Name() string { return t.spec.Name }

// Call implements tool.Tool. input is a JSON object of template values
// (strings, numbers, or booleans); it may be empty when the template has
// no placeholders.
func (t *cliTool) Call(ctx context.Context, _ tool.Runtime, input []byte) ([]byte, error) {
	vals, err := decode(input)
	if err != nil {
		return nil, err
	}
	argv, err := render(t.spec.Args, vals, t.spec.AllowFlagValues)
	if err != nil {
		return nil, err
	}

	dir := t.spec.Dir
	if dir == "" {
		tmp, err := os.MkdirTemp("", "playscape-cli-*")
		if err != nil {
			return nil, fmt.Errorf("cli: %s: workdir: %w", t.spec.Name, err)
		}
		defer os.RemoveAll(tmp)
		dir = tmp
	}

	ctx, cancel := context.WithTimeout(ctx, t.spec.Timeout)
	defer cancel()

	stdout := &capped{max: t.spec.MaxOutputBytes}
	stderr := &capped{max: t.spec.MaxOutputBytes}
	cmd := exec.CommandContext(ctx, t.bin, argv...)
	cmd.Dir = dir
	cmd.Env = t.env()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay

	runErr := cmd.Run()
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %s after %s", ErrTimeout, t.spec.Name, t.spec.Timeout)
		}
		return nil, fmt.Errorf("cli: %s: %w", t.spec.Name, ctx.Err())
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return nil, fmt.Errorf("cli: %s: %w", t.spec.Name, runErr)
	}

	out := Output{
		ExitCode:        cmd.ProcessState.ExitCode(),
		Stdout:          stdout.buf.String(),
		Stderr:          stderr.buf.String(),
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
	}
	return json.Marshal(out)
}

// env builds the child environment from the allowlist only.
func (t *cliTool) env() []string {
	env := make([]string, 0, len(t.spec.Env)+len(t.spec.SetEnv))
	for _, name := range t.spec.Env {
		if _, fixed := t.spec.SetEnv[name]; fixed {
			continue
		}
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	for k, v := range t.spec.SetEnv {
		env = append(env, k+"="+v)
	}
	return env
}

func decode(input []byte) (map[string]string, error) {
	vals := map[string]string{}
	if len(bytes.TrimSpace(input)) == 0 {
		return vals, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(input, &raw); err != nil {
		return nil, fmt.Errorf("%w: want a JSON object: %w", ErrInvalidInput, err)
	}
	for k, v := range raw {
		switch v := v.(type) {
		case string:
			vals[k] = v
		case float64:
			vals[k] = strconv.FormatFloat(v, 'f', -1, 64)
		case bool:
			vals[k] = strconv.FormatBool(v)
		default:
			return nil, fmt.Errorf("%w: %q must be a string, number, or boolean", ErrInvalidInput, k)
		}
	}
	return vals, nil
}

// render fills the argv template. Every placeholder must have a value;
// unknown input keys are rejected so typos fail loudly.
func render(tmpl []string, vals map[string]string, allowFlags bool) ([]string, error) {
	used := make(map[string]bool, len(vals))
	argv := make([]string, len(tmpl))
	var missing error
	for i, a := range tmpl {
		argv[i] = placeholder.ReplaceAllStringFunc(a, func(m string) string {
			name := m[1 : len(m)-1]
			v, ok := vals[name]
			if !ok {
				missing = fmt.Errorf("%w: missing value for {%s}", ErrInvalidInput, name)
				return m
			}
			if !allowFlags && strings.HasPrefix(v, "-") {
				missing = fmt.Errorf("%w: value for {%s} may not start with '-'", ErrInvalidInput, name)
				return m
			}
			if strings.ContainsRune(v, 0) {
				missing = fmt.Errorf("%w: value for {%s} contains NUL", ErrInvalidInput, name)
				return m
			}
			used[name] = true
			return v
		})
		if missing != nil {
			return nil, missing
		}
	}
	for k := range vals {
		if !used[k] {
			return nil, fmt.Errorf("%w: unknown parameter %q", ErrInvalidInput, k)
		}
	}
	return argv, nil
}

// capped is an io.Writer that keeps the first max bytes and discards the
// rest, recording that it did. It never returns an error, so the child is
// never killed by a full buffer.
type capped struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room < len(p) {
		c.truncated = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}
