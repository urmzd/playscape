package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// runCLI runs the command line and returns its exit code, stdout and stderr.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the tests drive /bin/sh")
	}
	var stdout, stderr bytes.Buffer
	code := execute(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunStagesGrantedFilesAndStreamsOutput(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "in", "a.txt"), "visible")
	writeFile(t, filepath.Join(ws, "secret", "key"), "hidden")

	code, stdout, stderr := runCLI(t, "run", "-workspace", ws, "-read", "in",
		`cat in/a.txt; test -e secret/key && echo LEAK; echo to-stderr >&2`)
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "visible") || !strings.Contains(stdout, "to-stderr") {
		t.Fatalf("output not streamed: %q", stdout)
	}
	if strings.Contains(stdout, "LEAK") {
		t.Fatal("a key outside the grant was staged")
	}
	if !strings.Contains(stderr, "playscape: ok") {
		t.Fatalf("report: %s", stderr)
	}
}

func TestRunAppliesAllowedChangesAndReportsDenied(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "out", "old"), "stale")

	code, _, stderr := runCLI(t, "run", "-workspace", ws, "-area", "out",
		`echo new > out/new; rm out/old; mkdir leak; echo x > leak/x`)
	if code != exitDenied {
		t.Fatalf("exit %d, want %d\n%s", code, exitDenied, stderr)
	}
	if got, err := os.ReadFile(filepath.Join(ws, "out", "new")); err != nil || string(got) != "new\n" {
		t.Fatalf("allowed write not applied: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(ws, "out", "old")); !os.IsNotExist(err) {
		t.Fatalf("allowed delete not applied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "leak")); !os.IsNotExist(err) {
		t.Fatalf("denied write reached the workspace: %v", err)
	}
	for _, want := range []string{"wrote out/new", "deleted out/old", "DENIED write leak/x"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("report missing %q:\n%s", want, stderr)
		}
	}
}

func TestRunMakesGrantedAreasDirectories(t *testing.T) {
	ws := t.TempDir()
	code, _, stderr := runCLI(t, "run", "-workspace", ws, "-area", "out/reports", "echo r > out/reports/r")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if got, err := os.ReadFile(filepath.Join(ws, "out", "reports", "r")); err != nil || string(got) != "r\n" {
		t.Fatalf("write into an empty granted area: %q, %v", got, err)
	}
}

func TestRunReadOnlyAreaRefusesChanges(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "in", "a"), "v1")

	code, _, stderr := runCLI(t, "run", "-workspace", ws, "-read", "in", `echo v2 > in/a; rm -f in/b`)
	if code != exitDenied {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "in", "a")); string(got) != "v1" {
		t.Fatalf("read-only key changed to %q", got)
	}
}

func TestRunReportsExitCodeTimeoutAndUsage(t *testing.T) {
	if code, _, stderr := runCLI(t, "run", "exit 3"); code != 3 || !strings.Contains(stderr, "exit code 3") {
		t.Fatalf("exit code: %d\n%s", code, stderr)
	}
	if code, _, stderr := runCLI(t, "run", "-timeout", "200ms", "sleep 5"); code != exitTimeout || !strings.Contains(stderr, "TIMED OUT") {
		t.Fatalf("timeout: %d\n%s", code, stderr)
	}
	for _, args := range [][]string{
		{"run"},
		{"run", "-area", "../up", "true"},
		{"run", "-policy", "p.json", "-area", "x", "true"},
		{"nope"},
	} {
		if code, _, _ := runCLI(t, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}

func TestRunEnvIsAllowlisted(t *testing.T) {
	t.Setenv("PLAYSCAPE_TEST_SECRET", "leak")
	t.Setenv("PLAYSCAPE_TEST_PASSED", "passed")
	code, stdout, stderr := runCLI(t, "run", "-env", "PLAYSCAPE_TEST_PASSED", "-env", "SET=fixed",
		`echo "[$PLAYSCAPE_TEST_SECRET] $PLAYSCAPE_TEST_PASSED $SET"`)
	if code != 0 || !strings.Contains(stdout, "[] passed fixed") {
		t.Fatalf("exit %d, stdout %q\n%s", code, stdout, stderr)
	}
}

func TestRunWithPolicyFileAndJSON(t *testing.T) {
	ws := t.TempDir()
	policy := filepath.Join(t.TempDir(), "policy.json")
	writeFile(t, policy, `{
	  "policy_classes": ["pc"],
	  "user_attrs": [{"name": "builders"}],
	  "users": [{"name": "ci", "parents": ["builders"]}],
	  "object_attrs": [{"name": "out", "prefix": "out", "parents": ["pc"]}],
	  "associations": [{"user_attr": "builders", "ops": ["read", "write"], "target": "out"}],
	  "prohibitions": [{"user_attr": "ci", "ops": ["write"], "target": "out"}]
	}`)

	code, stdout, _ := runCLI(t, "run", "-json", "-workspace", ws, "-policy", policy, "-as", "ci",
		`echo x > out/x; ln -s /etc/hosts link; echo printed`)
	if code != exitDenied {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	var rep report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("stdout is not one JSON report: %v\n%s", err, stdout)
	}
	if rep.User != "ci" || len(rep.Denied) != 1 || rep.Denied[0].Key != "out/x" {
		t.Fatalf("prohibition did not override the grant: %+v", rep)
	}
	if !slices.Equal(rep.Skipped, []string{"link"}) {
		t.Fatalf("symlink not skipped: %+v", rep.Skipped)
	}
	if rep.Output != "printed\n" {
		t.Fatalf("output = %q", rep.Output)
	}

	if code, _, _ := runCLI(t, "run", "-policy", writeBad(t), "true"); code != exitUsage {
		t.Fatalf("unknown policy field: exit %d, want %d", code, exitUsage)
	}
}

func writeBad(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "bad.json")
	writeFile(t, p, `{"policy_classes": ["pc"], "asociations": []}`)
	return p
}
