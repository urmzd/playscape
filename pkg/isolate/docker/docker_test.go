package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/playscape/pkg/isolate"
	"github.com/urmzd/playscape/pkg/isolate/isolatetest"
)

// testImage has a shell, busybox tools and python3. Override it with
// PLAYSCAPE_DOCKER_TEST_IMAGE.
const testImage = "python:3.13-alpine"

// The arguments every container starts with carry the whole boundary, so
// they are checked without a daemon.
func TestRunArgsLockTheContainerDown(t *testing.T) {
	d := &Driver{bin: "docker", runtime: RuntimeGVisor, idle: []string{"sleep", "infinity"}}
	args := d.runArgs("i-1", container{
		name: "playscape-i-1", compartment: isolate.Workload, image: "img", user: "1000:1000",
		resources: isolate.Resources{CPUMillis: 250}, network: "none", workDir: "/work", mounts: []string{"type=bind,source=/w,target=/work"},
	}, map[string]string{"team": "a"})
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{
		" --runtime runsc ", " --network none ", " --read-only ", " --cap-drop ALL ",
		" --security-opt no-new-privileges ", " --security-opt seccomp=/dev/stdin ", " --user 1000:1000 ", " --pids-limit 1088 ",
		" --memory 536870912 ", " --memory-swap 536870912 ", " --cpus 0.250 ",
		" --tmpfs /tmp:rw,nosuid,nodev,mode=1777,size=268435456 ", " --init ",
		" --label playscape.instance=i-1 ", " --label playscape.compartment=workload ", " --label team=a ",
		" --mount type=bind,source=/w,target=/work ", " --entrypoint sleep img infinity ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("run args lack %q:\n%s", want, joined)
		}
	}
}

// Production cannot fall back to a runtime that shares the host kernel
// without saying so.
func TestNewRequiresASandboxRuntime(t *testing.T) {
	bin, err := os.Executable() // any binary: New does not run it
	if err != nil {
		t.Fatal(err)
	}
	for _, rt := range []string{RuntimeRunc, "crun", "io.containerd.runc.v2"} {
		if _, err := New(Config{Binary: bin, Runtime: rt}); !errors.Is(err, ErrInsecureRuntime) {
			t.Errorf("%s: err = %v, want ErrInsecureRuntime", rt, err)
		}
	}
	d, err := New(Config{Binary: bin})
	if err != nil || d.runtime != RuntimeGVisor {
		t.Fatalf("default runtime: %v, %v", d, err)
	}
	var log strings.Builder
	d, err = New(Config{Binary: bin, Runtime: RuntimeRunc, InsecureDevelopmentRuntime: true, Log: &log})
	if err != nil || d.runtime != RuntimeRunc {
		t.Fatalf("opted in: %v, %v", d, err)
	}
	if !strings.Contains(log.String(), "NOT a boundary") {
		t.Fatalf("no warning: %q", log.String())
	}
}

// The profile is Docker's default with the denied syscalls never allowed,
// whatever capabilities a rule is conditioned on.
func TestSeccompProfileDeniesTheListedSyscalls(t *testing.T) {
	var p struct {
		DefaultAction   string `json:"defaultAction"`
		DefaultErrnoRet *int   `json:"defaultErrnoRet"`
		Syscalls        []struct {
			Names  []string          `json:"names"`
			Action string            `json:"action"`
			Args   []json.RawMessage `json:"args"`
		} `json:"syscalls"`
	}
	if err := json.Unmarshal(seccompProfile, &p); err != nil {
		t.Fatal(err)
	}
	if p.DefaultAction != "SCMP_ACT_ERRNO" || p.DefaultErrnoRet == nil || *p.DefaultErrnoRet != 1 {
		t.Fatalf("default: %s %v, want SCMP_ACT_ERRNO with EPERM", p.DefaultAction, p.DefaultErrnoRet)
	}
	allowed := map[string]bool{}
	for _, r := range p.Syscalls {
		for _, n := range r.Names {
			if r.Action != "SCMP_ACT_ALLOW" {
				continue
			}
			if slices.Contains(deniedSyscalls, n) {
				t.Errorf("%s is allowed", n)
			}
			if n == "personality" && len(r.Args) == 0 {
				t.Error("personality is allowed with any argument")
			}
			allowed[n] = true
		}
	}
	// What a workload needs is still there.
	for _, n := range []string{"read", "write", "execve", "clone", "openat", "socket", "personality"} {
		if !allowed[n] {
			t.Errorf("%s is not allowed", n)
		}
	}
}

// Under gVisor the host cgroup's PIDs limit bounds gVisor's own threads,
// so it gets room for them; the workload's limit is set in the sandbox.
func TestHostPIDsLimit(t *testing.T) {
	if got := (&Driver{runtime: RuntimeRunc}).hostPIDs(64); got != 64 {
		t.Errorf("runc: %d", got)
	}
	if got := (&Driver{runtime: RuntimeGVisor}).hostPIDs(64); got != 64*gvisorThreadsPerTask+gvisorThreadsBase {
		t.Errorf("runsc: %d", got)
	}
}

func TestBindRefusesCSVInjection(t *testing.T) {
	if _, err := bind("/a,target=/etc", "/work", false); !errors.Is(err, isolate.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
	m, err := bind("/p", "/project", true)
	if err != nil || m != "type=bind,source=/p,target=/project,readonly" {
		t.Fatalf("%q, %v", m, err)
	}
}

func TestDefaultUserIsNeverRoot(t *testing.T) {
	if got := defaultUser("7:7"); got != "7:7" {
		t.Fatalf("explicit user replaced: %q", got)
	}
	uid, _, err := isolate.ParseUser(defaultUser(""))
	if err != nil || uid <= 0 {
		t.Fatalf("default user %q: %v", defaultUser(""), err)
	}
}

func TestCLIFailures(t *testing.T) {
	for _, c := range []struct {
		code   int
		stderr string
		want   bool
	}{
		{127, "OCI runtime exec failed: exec failed: executable file not found", true},
		{127, "sh: nope: not found", false},
		{1, "Error response from daemon: container x is not running", true},
		{1, "some failure", false},
		{3, "Error response from daemon:", false},
	} {
		if got := cliFailed(c.code, c.stderr); got != c.want {
			t.Errorf("cliFailed(%d, %q) = %v", c.code, c.stderr, got)
		}
	}
}

func TestStartRefusesWhatItCannotEnforce(t *testing.T) {
	d := &Driver{bin: "docker", runtime: RuntimeRunc}
	for _, spec := range []isolate.Spec{
		{Image: "img", Network: isolate.Network{Proxy: "gate:3128"}},
		{Image: "img", Connectors: &isolate.ConnectorSpec{Network: isolate.Network{Proxy: "gate:3128"}}},
	} {
		if _, err := d.Start(context.Background(), spec); !errors.Is(err, isolate.ErrUnsupported) {
			t.Fatalf("a proxy without an egress network: err = %v", err)
		}
	}
}

// dockerDriver returns a driver against a reachable daemon with the test
// image pulled, or skips; with PLAYSCAPE_DOCKER_TEST_REQUIRED set it fails
// instead, so a CI job cannot pass by skipping.
func dockerDriver(t *testing.T, cfg Config) (*Driver, isolatetest.Options) {
	t.Helper()
	skip := t.Skip
	if os.Getenv("PLAYSCAPE_DOCKER_TEST_REQUIRED") != "" {
		skip = t.Fatal
	}
	if testing.Short() {
		skip("docker tests are skipped in -short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		skip("docker is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		skip("the docker daemon is not reachable")
	}
	image := os.Getenv("PLAYSCAPE_DOCKER_TEST_IMAGE")
	if image == "" {
		image = testImage
	}
	if exec.Command("docker", "image", "inspect", image).Run() != nil {
		if out, err := exec.Command("docker", "pull", image).CombinedOutput(); err != nil {
			skip(fmt.Sprintf("cannot pull %s: %v: %s", image, err, out))
		}
	}
	// Docker Desktop and Colima share only some host directories with
	// their VM; the user cache directory is under HOME, which they do.
	base := os.Getenv("PLAYSCAPE_DOCKER_TEST_DIR")
	if base == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		base = filepath.Join(cache, "playscape-test")
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	tempDir := func(t testing.TB) string {
		dir, err := os.MkdirTemp(base, "t-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		return dir
	}
	cfg.WorkRoot = tempDir(t)
	// The runtime under test: runc by default, which needs the
	// development opt-in, or PLAYSCAPE_DOCKER_TEST_RUNTIME (runsc in CI).
	if cfg.Runtime == "" {
		cfg.Runtime = os.Getenv("PLAYSCAPE_DOCKER_TEST_RUNTIME")
	}
	if cfg.Runtime == "" {
		cfg.Runtime = RuntimeRunc
	}
	cfg.InsecureDevelopmentRuntime = !sandboxRuntimes[cfg.Runtime]
	cfg.Log = io.Discard
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return d, isolatetest.Options{
		Image:    image,
		Env:      map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin"},
		TempDir:  tempDir,
		Isolated: true,
	}
}

func TestConformance(t *testing.T) {
	d, opts := dockerDriver(t, Config{})
	isolatetest.Run(t, d, opts)
	assertNoLeftovers(t, d)
}

// Every container runs under the driver's runtime.
func TestRuntimeIsHonored(t *testing.T) {
	d, opts := dockerDriver(t, Config{})
	inst, err := d.Start(context.Background(), isolate.Spec{Image: opts.Image, Connectors: &isolate.ConnectorSpec{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inst.Close(context.Background()) }()
	i := inst.(*instance)
	out, err := exec.Command("docker", "inspect", "--format", "{{.HostConfig.Runtime}}", i.workload, i.connectors).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(out)); len(got) != 2 || got[0] != d.runtime || got[1] != d.runtime {
		t.Fatalf("runtimes %q, want %s", got, d.runtime)
	}
	t.Logf("runtime %s", d.runtime)
}

// A runtime the daemon does not have fails the start, so the runtime flag
// is passed and honored rather than silently dropped.
func TestUnknownRuntimeFailsToStart(t *testing.T) {
	d, opts := dockerDriver(t, Config{Runtime: "playscape-no-such-runtime"})
	inst, err := d.Start(context.Background(), isolate.Spec{Image: opts.Image})
	if !errors.Is(err, isolate.ErrStart) {
		if inst != nil {
			_ = inst.Close(context.Background())
		}
		t.Fatalf("err = %v, want ErrStart", err)
	}
	assertNoLeftovers(t, d)
}

func TestCloseRemovesEverything(t *testing.T) {
	d, opts := dockerDriver(t, Config{})
	inst, err := d.Start(context.Background(), isolate.Spec{Image: opts.Image, Connectors: &isolate.ConnectorSpec{}})
	if err != nil {
		t.Fatal(err)
	}
	root := inst.(*instance).root
	if err := inst.Me().Write(context.Background(), "memory/note.md", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if err := inst.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instance directory survived: %v", err)
	}
	assertNoLeftovers(t, d)
}

// A pool over the real driver: leases are fresh instances, and nothing is
// left once leases are released and the pool is closed.
func TestPoolOverDocker(t *testing.T) {
	d, opts := dockerDriver(t, Config{})
	pool := isolate.NewPool(d, isolate.PoolConfig{Warm: 2, Max: 4})
	spec := isolate.Spec{Image: opts.Image}
	ctx := context.Background()
	seen := map[string]bool{}
	for range 4 {
		l, err := pool.Lease(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if seen[l.ID()] {
			t.Fatalf("instance %s leased twice", l.ID())
		}
		seen[l.ID()] = true
		st, err := l.Exec(ctx, isolate.Command{Argv: []string{"/bin/sh", "-c", "test ! -e marker && touch marker"}}, isolate.Stdio{})
		if err != nil || st.Code != 0 {
			t.Fatalf("a leased instance was not fresh: %+v, %v", st, err)
		}
		if err := l.Release(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.Close(ctx); err != nil {
		t.Fatal(err)
	}
	assertNoLeftovers(t, d)
}

// assertNoLeftovers checks that no playscape container, volume or work
// directory is left. The docker tests run one at a time, so any container
// with the instance label is a leak.
func assertNoLeftovers(t *testing.T, d *Driver) {
	t.Helper()
	for _, args := range [][]string{
		{"ps", "--all", "--filter", "label=" + InstanceLabel, "--format", "{{.Names}}"},
		{"volume", "ls", "--filter", "label=" + InstanceLabel, "--format", "{{.Name}}"},
	} {
		out, err := exec.Command("docker", args...).Output()
		if err != nil {
			t.Fatal(err)
		}
		if left := strings.TrimSpace(string(out)); left != "" {
			t.Errorf("left behind: %s", left)
		}
	}
	if entries, _ := os.ReadDir(d.workRoot); len(entries) > 0 {
		t.Errorf("work directories left behind: %d", len(entries))
	}
}
