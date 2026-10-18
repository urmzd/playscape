package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/playscape/pkg/tool"
)

func newTool(t *testing.T, s Spec) tool.Tool {
	t.Helper()
	tools, err := New(s).Tools()
	if err != nil {
		t.Fatal(err)
	}
	return tools[0]
}

func call(t *testing.T, tl tool.Tool, input string) (Output, error) {
	t.Helper()
	raw, err := tl.Call(context.Background(), nil, []byte(input))
	if err != nil {
		return Output{}, err
	}
	var out Output
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return out, nil
}

func TestProviderKindAndRegistration(t *testing.T) {
	p := New(Spec{Name: "echo", Binary: "echo"}, Spec{Name: "sh", Binary: "/bin/sh"})
	if p.Kind() != tool.KindCLI {
		t.Fatalf("kind = %q", p.Kind())
	}
	r := tool.NewRegistry()
	if err := r.RegisterProvider(p); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.Names(), ","); got != "echo,sh" {
		t.Fatalf("names = %q", got)
	}
}

func TestInvalidSpec(t *testing.T) {
	for name, s := range map[string]Spec{
		"no name":     {Binary: "echo"},
		"no binary":   {Name: "x"},
		"missing bin": {Name: "x", Binary: "definitely-not-a-binary-playscape"},
		"bad env":     {Name: "x", Binary: "echo", Env: []string{"A=B"}},
		"neg timeout": {Name: "x", Binary: "echo", Timeout: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(s).Tools(); !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("err = %v, want ErrInvalidSpec", err)
			}
		})
	}
}

func TestArgvTemplateIsNotShellExpanded(t *testing.T) {
	tl := newTool(t, Spec{Name: "echo", Binary: "echo", Args: []string{"hello", "--to={who}", "{n}"}})
	out, err := call(t, tl, `{"who":"a b; rm -rf / $(id)","n":3}`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "hello --to=a b; rm -rf / $(id) 3\n"; out.Stdout != want {
		t.Fatalf("stdout = %q, want %q", out.Stdout, want)
	}
	if out.ExitCode != 0 {
		t.Fatalf("exit = %d", out.ExitCode)
	}
}

func TestInvalidInput(t *testing.T) {
	tl := newTool(t, Spec{Name: "echo", Binary: "echo", Args: []string{"{msg}"}})
	for name, in := range map[string]string{
		"missing value":   `{}`,
		"unknown key":     `{"msg":"hi","extra":"x"}`,
		"not an object":   `"hi"`,
		"nested value":    `{"msg":{"a":1}}`,
		"flag injection":  `{"msg":"--version"}`,
		"nul in argument": `{"msg":"a\u0000b"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := call(t, tl, in); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestAllowFlagValues(t *testing.T) {
	tl := newTool(t, Spec{Name: "echo", Binary: "echo", Args: []string{"x", "{msg}"}, AllowFlagValues: true})
	out, err := call(t, tl, `{"msg":"-y"}`)
	if err != nil || out.Stdout != "x -y\n" {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
}

func TestNonZeroExitIsData(t *testing.T) {
	tl := newTool(t, Spec{Name: "sh", Binary: "/bin/sh", Args: []string{"-c", "echo oops >&2; exit 7"}})
	out, err := call(t, tl, "")
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 7 || out.Stderr != "oops\n" {
		t.Fatalf("out = %+v", out)
	}
}

func TestEnvAllowlist(t *testing.T) {
	t.Setenv("PLAYSCAPE_ALLOWED", "yes")
	t.Setenv("PLAYSCAPE_SECRET", "leak")
	tl := newTool(t, Spec{
		Name:   "env",
		Binary: "/usr/bin/env",
		Env:    []string{"PLAYSCAPE_ALLOWED", "PLAYSCAPE_UNSET"},
		SetEnv: map[string]string{"FIXED": "1"},
	})
	out, err := call(t, tl, "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(out.Stdout), "\n") {
		got[l] = true
	}
	if !got["PLAYSCAPE_ALLOWED=yes"] || !got["FIXED=1"] || len(got) != 2 {
		t.Fatalf("env = %q, want only the allowlisted and fixed vars", out.Stdout)
	}
}

func TestTimeout(t *testing.T) {
	tl := newTool(t, Spec{Name: "sleep", Binary: "/bin/sh", Args: []string{"-c", "sleep 5"}, Timeout: 100 * time.Millisecond})
	start := time.Now()
	if _, err := call(t, tl, ""); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("timeout took %s", d)
	}
}

func TestOutputCap(t *testing.T) {
	tl := newTool(t, Spec{
		Name:           "yes",
		Binary:         "/bin/sh",
		Args:           []string{"-c", "i=0; while [ $i -lt 100 ]; do echo 0123456789; i=$((i+1)); done"},
		MaxOutputBytes: 25,
	})
	out, err := call(t, tl, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Stdout) != 25 || !out.StdoutTruncated || out.StderrTruncated {
		t.Fatalf("out = %+v", out)
	}
}

func TestDefaultWorkdirIsFreshAndRemoved(t *testing.T) {
	tl := newTool(t, Spec{Name: "pwd", Binary: "/bin/sh", Args: []string{"-c", "pwd; ls -A"}})
	out, err := call(t, tl, "")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.Stdout), "\n")
	if len(lines) != 1 {
		t.Fatalf("workdir not empty: %q", out.Stdout)
	}
	if _, err := os.Stat(lines[0]); !os.IsNotExist(err) {
		t.Fatalf("workdir %s not removed: %v", lines[0], err)
	}
}
