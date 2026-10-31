// Package docker is an isolate.Driver that runs each instance as Docker
// containers, driven through the docker CLI (no SDK dependency). The
// runtime is runsc (gVisor) unless the program opts into another: gVisor
// puts a user-space kernel between each instance and the host, which is
// the boundary for code you do not trust. runc shares the host kernel and
// isolates only with namespaces, cgroups, seccomp and no capabilities, so
// New accepts it only with Config.InsecureDevelopmentRuntime.
//
// Every container starts with no network (--network none), a read-only
// root filesystem, all capabilities dropped, no-new-privileges, a seccomp
// profile stricter than Docker's default (see deniedSyscalls), a non-root
// user, and CPU, memory (no swap), process and scratch-disk limits. The
// scratch directory /tmp is a size-limited tmpfs. The work directory is a
// host directory bind-mounted at /work, so Files reads and writes it
// directly; the project is bind-mounted read only at /project and the
// person volume read and write at /me.
//
// A spec with Connectors gets a second container that mounts none of the
// workload's directories and shares exactly one with it: a 1 MiB tmpfs
// Docker volume at /run/playscape, for unix sockets. The two containers
// have separate process, network and mount namespaces.
//
// Commands run with docker exec. Their environment variables reach the CLI
// through its own environment (docker exec -e NAME), never on its command
// line, so they do not show in the host's process list.
//
// # Remote daemons
//
// With Config.Host set, the driver drives that daemon instead of the local
// one, for example a sandbox host reached over TLS, so the program does not
// hold a Docker socket on its own machine. A remote daemon does not see this
// machine's files, so the work directory is a Docker volume on the daemon's
// machine instead of a host directory, and Files reaches it through the
// workload container: each call is one docker exec of a small shell script,
// run as the instance's user. Nothing outside the sandbox opens the run's
// files, so a link the workload plants is resolved inside it. Host
// directory mounts (Spec.Mounts) are refused with isolate.ErrUnsupported.
// The image needs sh, cat, mkdir, dirname, rm, find and chmod (busybox has
// them all).
//
// Orthogonality boundary: this package owns containers, its volumes and one
// host directory per instance; it knows nothing about pools or runs.
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urmzd/playscape/pkg/isolate"
	"github.com/urmzd/playscape/pkg/workspace"
)

// Runtimes the driver knows by name. Any other OCI runtime registered with
// the daemon can be named in Config.Runtime too, with
// Config.InsecureDevelopmentRuntime.
const (
	RuntimeRunc   = "runc"
	RuntimeGVisor = "runsc"
)

// sandboxRuntimes are the runtimes that give each container a kernel of its
// own, so New accepts them without InsecureDevelopmentRuntime. A microVM
// runtime joins this set once the conformance suite runs against it in CI.
var sandboxRuntimes = map[string]bool{RuntimeGVisor: true}

// ErrInsecureRuntime is returned by New for a runtime that shares the host
// kernel when Config.InsecureDevelopmentRuntime is not set.
var ErrInsecureRuntime = errors.New("isolate: docker: the runtime shares the host kernel")

// DefaultUser is the user containers run as when the spec names none and
// the host program runs as root. Otherwise the default is the host
// program's own uid and gid, so it can manage the files a run leaves in
// the work directory.
const DefaultUser = "65532:65532"

// Labels the driver sets on every container and volume.
const (
	InstanceLabel    = isolate.LabelPrefix + "instance"
	CompartmentLabel = isolate.LabelPrefix + "compartment"
)

const (
	namePrefix  = "playscape-"
	socketBytes = 1 << 20
	// cleanupTimeout bounds the restart after a timed-out command.
	cleanupTimeout = 30 * time.Second
)

// Config configures a Driver.
type Config struct {
	// Binary is the docker CLI; empty means "docker" on PATH.
	Binary string
	// Runtime is the OCI runtime for every container; empty means
	// RuntimeGVisor, which the daemon must have registered.
	Runtime string
	// InsecureDevelopmentRuntime lets New accept a runtime that shares the
	// host kernel, such as RuntimeRunc. A kernel bug is then a way out of
	// every instance. Set it only where the code that runs is your own:
	// development machines without gVisor, and tests. New warns on Log
	// when it is set.
	InsecureDevelopmentRuntime bool
	// Log receives New's warning about an insecure runtime; nil means
	// os.Stderr.
	Log io.Writer
	// WorkRoot is the host directory instance work directories are
	// created in; empty means the system temporary directory. The Docker
	// daemon must be able to bind-mount it: with Docker Desktop or Colima
	// that means a directory shared with the VM. The driver does not
	// limit how much a run writes there; give WorkRoot a filesystem of
	// its own so a run cannot fill the host's disk.
	WorkRoot string
	// IdleCommand keeps a container running between commands; empty
	// means ["sleep", "infinity"]. The image must have it.
	IdleCommand []string
	// EgressNetwork is the Docker network a compartment whose Spec has a
	// Network.Proxy joins instead of none. It must be created with
	// --internal, so it has no route out, and the egress gate must be
	// reachable on it at the spec's proxy address. Empty refuses every
	// spec with a proxy (ErrUnsupported).
	EgressNetwork string
	// Host is the daemon to drive, passed to the CLI as --host:
	// "tcp://sandbox.internal:2376", "ssh://user@sandbox" or
	// "unix:///path/to/docker.sock". Empty means the CLI's default
	// daemon. A set Host is treated as remote: see Remote daemons in the
	// package doc. WorkRoot is then unused.
	Host string
	// TLSDir holds ca.pem, cert.pem and key.pem. With it the CLI checks
	// the daemon's certificate and presents its own (--tlsverify). A
	// tcp:// Host requires it: a daemon on plain TCP is root on its
	// machine for anyone who can reach the port.
	TLSDir string
}

// Driver starts Docker-backed instances. It is safe for concurrent use.
type Driver struct {
	bin      string
	global   []string // CLI flags before every subcommand: --host and TLS
	remote   bool     // the daemon does not share this machine's files
	runtime  string
	workRoot string
	idle     []string
	env      []string // the CLI's own environment
	egress   string

	egressMu sync.Mutex
	egressOK bool  // the network was found internal
	egressNo error // the network is not internal: never usable
}

var _ isolate.Driver = (*Driver)(nil)

var runtimeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// New returns a driver. It finds the CLI but does not contact the daemon.
func New(cfg Config) (*Driver, error) {
	if cfg.Runtime == "" {
		cfg.Runtime = RuntimeGVisor
	}
	if !runtimeName.MatchString(cfg.Runtime) {
		return nil, fmt.Errorf("isolate: docker: runtime %q", cfg.Runtime)
	}
	if !sandboxRuntimes[cfg.Runtime] {
		if !cfg.InsecureDevelopmentRuntime {
			return nil, fmt.Errorf("%w: %q (use %q, or set InsecureDevelopmentRuntime for development)", ErrInsecureRuntime, cfg.Runtime, RuntimeGVisor)
		}
		log := cfg.Log
		if log == nil {
			log = os.Stderr
		}
		fmt.Fprintf(log, "playscape: isolate/docker: runtime %q shares the host kernel: instances are NOT a boundary for untrusted code (InsecureDevelopmentRuntime is set)\n", cfg.Runtime)
	}
	if cfg.Binary == "" {
		cfg.Binary = "docker"
	}
	bin, err := exec.LookPath(cfg.Binary)
	if err != nil {
		return nil, fmt.Errorf("isolate: docker: %w", err)
	}
	global, err := hostArgs(cfg.Host, cfg.TLSDir)
	if err != nil {
		return nil, err
	}
	remote := cfg.Host != ""
	var root string
	if !remote {
		if cfg.WorkRoot == "" {
			cfg.WorkRoot = os.TempDir()
		}
		// Docker resolves bind sources on the daemon's side, which does
		// not follow the host's links (/tmp and /var on macOS are links).
		if root, err = filepath.EvalSymlinks(cfg.WorkRoot); err != nil {
			return nil, fmt.Errorf("isolate: docker: work root: %w", err)
		}
	}
	if len(cfg.IdleCommand) == 0 {
		cfg.IdleCommand = []string{"sleep", "infinity"}
	}
	if cfg.EgressNetwork != "" && !runtimeName.MatchString(cfg.EgressNetwork) {
		return nil, fmt.Errorf("isolate: docker: egress network %q", cfg.EgressNetwork)
	}
	return &Driver{
		bin: bin, global: global, remote: remote, runtime: cfg.Runtime, workRoot: root,
		idle: cfg.IdleCommand, env: cliEnv(remote), egress: cfg.EgressNetwork,
	}, nil
}

// hostArgs checks a Config's Host and TLSDir and returns the CLI flags that
// select them.
func hostArgs(host, tlsDir string) ([]string, error) {
	if host == "" {
		if tlsDir != "" {
			return nil, errors.New("isolate: docker: TLSDir without a Host")
		}
		return nil, nil
	}
	scheme, rest, ok := strings.Cut(host, "://")
	if !ok || rest == "" || strings.ContainsAny(host, " \t\n\x00") {
		return nil, fmt.Errorf("isolate: docker: host %q: want tcp://, ssh:// or unix://", host)
	}
	switch scheme {
	case "tcp":
		if tlsDir == "" {
			return nil, fmt.Errorf("isolate: docker: host %q: a tcp:// daemon needs TLSDir", host)
		}
	case "ssh", "unix":
	default:
		return nil, fmt.Errorf("isolate: docker: host %q: want tcp://, ssh:// or unix://", host)
	}
	args := []string{"--host", host}
	if tlsDir == "" {
		return args, nil
	}
	if !filepath.IsAbs(tlsDir) {
		return nil, fmt.Errorf("isolate: docker: TLSDir %q: want an absolute path", tlsDir)
	}
	args = append(args, "--tlsverify")
	for _, f := range []struct{ flag, file string }{{"--tlscacert", "ca.pem"}, {"--tlscert", "cert.pem"}, {"--tlskey", "key.pem"}} {
		p := filepath.Join(tlsDir, f.file)
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("isolate: docker: TLSDir: %w", err)
		}
		args = append(args, f.flag, p)
	}
	return args, nil
}

// cliEnv is the environment the docker CLI runs with: the variables it
// reads, and DOCKER_CONFIG pinned so a command's HOME cannot move it. With
// remote set the daemon comes from Config alone, so the variables that
// would pick another one are dropped.
func cliEnv(remote bool) []string {
	var env []string
	hasConfig := false
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case remote && (k == "DOCKER_HOST" || k == "DOCKER_CONTEXT" || k == "DOCKER_TLS_VERIFY" || k == "DOCKER_CERT_PATH"):
			continue
		case strings.HasPrefix(k, "DOCKER_"):
			hasConfig = hasConfig || k == "DOCKER_CONFIG"
		case k == "HOME", k == "PATH", k == "TMPDIR", k == "USER", k == "SSH_AUTH_SOCK", strings.HasPrefix(k, "XDG_"):
		default:
			continue
		}
		env = append(env, kv)
	}
	if home, err := os.UserHomeDir(); err == nil && !hasConfig {
		env = append(env, "DOCKER_CONFIG="+filepath.Join(home, ".docker"))
	}
	return env
}

// Start implements isolate.Driver.
func (d *Driver) Start(ctx context.Context, spec isolate.Spec) (isolate.Instance, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.Network.Proxy != "" || (spec.Connectors != nil && spec.Connectors.Network.Proxy != "") {
		if err := d.egressReady(ctx); err != nil {
			return nil, err
		}
	}
	if d.remote {
		return d.startRemote(ctx, spec)
	}
	mounts, err := resolveMounts(spec.Mounts)
	if err != nil {
		return nil, err
	}
	user := defaultUser(spec.User)
	id := isolate.NewID()
	inst := &instance{
		d:        d,
		id:       id,
		workload: namePrefix + id,
		work:     filepath.Join(d.workRoot, namePrefix+id),
	}
	if err := os.Mkdir(inst.work, 0o700); err != nil {
		return nil, fmt.Errorf("%w: docker: work directory: %w", isolate.ErrStart, err)
	}
	if uid, _, _ := isolate.ParseUser(user); uid != os.Getuid() {
		// The container's user is not the directory's owner.
		if err := os.Chmod(inst.work, 0o777); err != nil { //nolint:gosec // the run's own directory
			os.RemoveAll(inst.work)
			return nil, fmt.Errorf("%w: docker: work directory: %w", isolate.ErrStart, err)
		}
	}
	if err := inst.start(ctx, spec, mounts, user); err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_ = inst.destroy(cctx)
		return nil, err
	}
	return inst, nil
}

// egressReady checks, once, that the egress network exists and is internal:
// a network with a route out would let a compartment around the gate.
func (d *Driver) egressReady(ctx context.Context) error {
	if d.egress == "" {
		return fmt.Errorf("%w: docker: egress through a proxy needs Config.EgressNetwork", isolate.ErrUnsupported)
	}
	d.egressMu.Lock()
	defer d.egressMu.Unlock()
	if d.egressOK || d.egressNo != nil {
		return d.egressNo
	}
	out, err := d.cli(ctx, nil, "network", "inspect", "--format", "{{.Internal}}", d.egress)
	if err != nil {
		return fmt.Errorf("%w: docker: egress network: %w", isolate.ErrStart, err) // may pass; checked again
	}
	if strings.TrimSpace(string(out)) != "true" {
		d.egressNo = fmt.Errorf("%w: docker: egress network %q is not --internal, so it has a route around the gate", isolate.ErrUnsupported, d.egress)
		return d.egressNo
	}
	d.egressOK = true
	return nil
}

// network is the --network value for a compartment with n.
func (d *Driver) network(n isolate.Network) string {
	if n.Proxy != "" {
		return d.egress
	}
	return "none"
}

// resolveMounts checks that every mount is a directory and resolves links,
// which the daemon would not follow.
func resolveMounts(m isolate.Mounts) (isolate.Mounts, error) {
	resolve := func(dir string) (string, error) {
		if dir == "" {
			return "", nil
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			var info os.FileInfo
			if info, err = os.Stat(resolved); err == nil && !info.IsDir() {
				err = errors.New("not a directory")
			}
		}
		if err != nil {
			return "", fmt.Errorf("%w: docker: mount %q: %w", isolate.ErrStart, dir, err)
		}
		return resolved, nil
	}
	var err error
	if m.Project, err = resolve(m.Project); err != nil {
		return m, err
	}
	m.Me, err = resolve(m.Me)
	return m, err
}

// defaultUser returns user, or the default when it is empty.
func defaultUser(user string) string {
	if user != "" {
		return user
	}
	if uid := os.Getuid(); uid > 0 {
		if gid := os.Getgid(); gid > 0 {
			return strconv.Itoa(uid) + ":" + strconv.Itoa(gid)
		}
		return strconv.Itoa(uid)
	}
	return DefaultUser
}

// container describes one container of an instance, for runArgs.
type container struct {
	name        string
	compartment isolate.Compartment
	image       string
	user        string
	resources   isolate.Resources
	network     string // "none" or the egress network
	workDir     string
	mounts      []string // --mount values
}

// runArgs is the docker run argv (after the binary) for c.
func (d *Driver) runArgs(id string, c container, labels map[string]string) []string {
	r := c.resources.WithDefaults()
	args := []string{
		"run", "--detach", "--init",
		"--name", c.name,
		"--runtime", d.runtime,
		"--network", c.network,
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--security-opt", "seccomp=/dev/stdin", // seccompProfile, on stdin
		"--user", c.user,
		"--pids-limit", strconv.FormatInt(d.hostPIDs(r.PIDs), 10),
		"--memory", strconv.FormatInt(r.MemoryBytes, 10),
		"--memory-swap", strconv.FormatInt(r.MemoryBytes, 10),
		"--cpus", strconv.FormatFloat(float64(r.CPUMillis)/1000, 'f', 3, 64),
		"--tmpfs", "/tmp:rw,nosuid,nodev,mode=1777,size=" + strconv.FormatInt(r.DiskBytes, 10),
		"--log-driver", "none",
		"--workdir", c.workDir,
		"--label", InstanceLabel + "=" + id,
		"--label", CompartmentLabel + "=" + c.compartment.String(),
	}
	for _, k := range sortedKeys(labels) {
		args = append(args, "--label", k+"="+labels[k])
	}
	for _, m := range c.mounts {
		args = append(args, "--mount", m)
	}
	args = append(args, "--entrypoint", d.idle[0], c.image)
	return append(args, d.idle[1:]...)
}

// gVisor runs a sandbox's processes as its own tasks, on host threads of
// the sandbox: about 35 when idle and 2 more per task. Docker's PIDs limit
// is the host cgroup's, so under gVisor it bounds those threads, and a
// workload reaching it starves gVisor and ends the instance. The driver
// instead enforces the limit inside the sandbox (guestPIDs) and gives the
// host cgroup room for gVisor's own threads.
const (
	gvisorThreadsPerTask = 4
	gvisorThreadsBase    = 64
)

// hostPIDs is the --pids-limit for a compartment limited to pids.
func (d *Driver) hostPIDs(pids int64) int64 {
	if d.runtime == RuntimeGVisor {
		return pids*gvisorThreadsPerTask + gvisorThreadsBase
	}
	return pids
}

// guestPIDs sets the PIDs limit inside a gVisor sandbox: its cgroupfs's
// pids.max, which gVisor enforces when a task is created (EAGAIN). It runs
// as root, the owner of the file, with the image's /bin/sh; the workload's
// user cannot change it. Other runtimes need nothing: the host cgroup's
// limit is the processes' own.
func (d *Driver) guestPIDs(ctx context.Context, name string, pids int64) error {
	if d.runtime != RuntimeGVisor {
		return nil
	}
	_, err := d.cli(ctx, nil, "exec", "--user", "0", name, "/bin/sh", "-c",
		`echo "$1" > /sys/fs/cgroup/pids/pids.max && [ "$(cat /sys/fs/cgroup/pids/pids.max)" = "$1" ]`, "sh", strconv.FormatInt(pids, 10))
	if err != nil {
		return fmt.Errorf("setting the PIDs limit in the sandbox: %w", err)
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}

// bind returns a --mount value for a bind mount. Docker's --mount syntax
// is CSV, so a source with a comma or quote is refused rather than escaped.
func bind(src, dst string, readOnly bool) (string, error) {
	if strings.ContainsAny(src, ",\"\n") {
		return "", fmt.Errorf("%w: docker: mount source %q has a comma or quote", isolate.ErrUnsupported, src)
	}
	m := "type=bind,source=" + src + ",target=" + dst
	if readOnly {
		m += ",readonly"
	}
	return m, nil
}

type instance struct {
	d          *Driver
	id         string
	workload   string // container names
	connectors string
	volume     string // socket volume, when there are connectors
	work       string // host work directory, with a local daemon
	workVolume string // work volume, with a remote daemon
	files      workspace.Workspace
	local      *workspace.Local // files, when they are a host directory
	pids       map[string]int64 // each container's PIDs limit, by name

	mu      sync.Mutex
	closed  bool
	running sync.WaitGroup
	once    sync.Once
	err     error
}

func (i *instance) start(ctx context.Context, spec isolate.Spec, m isolate.Mounts, user string) error {
	var work string
	if i.workVolume != "" {
		_, err := i.d.cli(ctx, nil, "volume", "create", "--driver", "local", "--label", InstanceLabel+"="+i.id, i.workVolume)
		if err != nil {
			return fmt.Errorf("%w: docker: work volume: %w", isolate.ErrStart, err)
		}
		work = "type=volume,source=" + i.workVolume + ",target=" + isolate.WorkDir
	} else {
		var err error
		if work, err = bind(i.work, isolate.WorkDir, false); err != nil {
			return err
		}
	}
	mounts := []string{work}
	if m.Project != "" {
		b, err := bind(m.Project, isolate.ProjectDir, true)
		if err != nil {
			return err
		}
		mounts = append(mounts, b)
	}
	if m.Me != "" {
		b, err := bind(m.Me, isolate.MeDir, false)
		if err != nil {
			return err
		}
		mounts = append(mounts, b)
	}
	var connector container
	if c := spec.Connectors; c != nil {
		i.volume = namePrefix + i.id + "-sockets"
		_, err := i.d.cli(ctx, nil, "volume", "create", "--driver", "local",
			"--opt", "type=tmpfs", "--opt", "device=tmpfs",
			"--opt", "o=size="+strconv.Itoa(socketBytes)+",mode=1777",
			"--label", InstanceLabel+"="+i.id, i.volume)
		if err != nil {
			return fmt.Errorf("%w: docker: socket volume: %w", isolate.ErrStart, err)
		}
		sock := "type=volume,source=" + i.volume + ",target=" + isolate.SocketDir
		mounts = append(mounts, sock)
		image := c.Image
		if image == "" {
			image = spec.Image
		}
		i.connectors = namePrefix + i.id + "-connectors"
		connector = container{
			name: i.connectors, compartment: isolate.Connectors, image: image,
			user: i.d.userOr(c.User), resources: c.Resources, network: i.d.network(c.Network), workDir: "/tmp",
			mounts: []string{sock},
		}
	}
	workload := container{
		name: i.workload, compartment: isolate.Workload, image: spec.Image,
		user: user, resources: spec.Resources, network: i.d.network(spec.Network), workDir: isolate.WorkDir, mounts: mounts,
	}
	i.pids = map[string]int64{workload.name: workload.resources.WithDefaults().PIDs}
	if _, err := i.d.run(ctx, i.d.runArgs(i.id, workload, spec.Labels)); err != nil {
		return fmt.Errorf("%w: docker: %w", isolate.ErrStart, err)
	}
	if err := i.d.guestPIDs(ctx, workload.name, i.pids[workload.name]); err != nil {
		return fmt.Errorf("%w: docker: %w", isolate.ErrStart, err)
	}
	if i.workVolume != "" {
		// A new volume is owned by root. Root in the container has no
		// capabilities, but as the owner it may still open it up.
		if _, err := i.d.cli(ctx, nil, "exec", "--user", "0", workload.name, "chmod", "0777", isolate.WorkDir); err != nil {
			return fmt.Errorf("%w: docker: work volume: %w", isolate.ErrStart, err)
		}
	}
	if i.connectors != "" {
		i.pids[connector.name] = connector.resources.WithDefaults().PIDs
		if _, err := i.d.run(ctx, i.d.runArgs(i.id, connector, spec.Labels)); err != nil {
			return fmt.Errorf("%w: docker: connectors: %w", isolate.ErrStart, err)
		}
		if err := i.d.guestPIDs(ctx, connector.name, i.pids[connector.name]); err != nil {
			return fmt.Errorf("%w: docker: connectors: %w", isolate.ErrStart, err)
		}
	}
	if i.workVolume != "" {
		i.files = execFiles{i: i}
		return nil
	}
	files, err := workspace.NewLocal(i.work)
	if err != nil {
		return fmt.Errorf("%w: docker: %w", isolate.ErrStart, err)
	}
	i.files, i.local = files, files
	return nil
}

// run runs docker run with args from runArgs, which read the seccomp
// profile from standard input.
func (d *Driver) run(ctx context.Context, args []string) ([]byte, error) {
	return d.cliIn(ctx, bytes.NewReader(seccompProfile), nil, args...)
}

// cli runs the docker CLI with args and returns its stdout. extraEnv is
// added to the CLI's environment. A failure carries the CLI's stderr.
func (d *Driver) cli(ctx context.Context, extraEnv []string, args ...string) ([]byte, error) {
	return d.cliIn(ctx, nil, extraEnv, args...)
}

// cliIn is cli with stdin as the CLI's standard input.
func (d *Driver) cliIn(ctx context.Context, stdin io.Reader, extraEnv []string, args ...string) ([]byte, error) {
	cmd := d.command(ctx, args...)
	cmd.Env = append(append([]string{}, d.env...), extraEnv...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("docker %s: %w: %s", args[0], err, msg)
		}
		return nil, fmt.Errorf("docker %s: %w", args[0], err)
	}
	return stdout.Bytes(), nil
}

// command is the docker CLI with the daemon flags and then args.
func (d *Driver) command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, d.bin, append(slices.Clip(d.global), args...)...)
}

func (i *instance) ID() string                 { return i.id }
func (i *instance) Files() workspace.Workspace { return i.files }

// Exec implements isolate.Instance. A command that times out or whose ctx
// ends restarts its compartment: docker exec cannot end the processes it
// started any other way. The restart ends every process in that
// compartment, other running commands included, and clears its /tmp; the
// work directory and the mounts are kept.
func (i *instance) Exec(ctx context.Context, c isolate.Command, stdio isolate.Stdio) (isolate.ExitStatus, error) {
	if err := c.Validate(); err != nil {
		return isolate.ExitStatus{}, err
	}
	for k := range c.Env {
		if strings.HasPrefix(k, "DOCKER_") {
			return isolate.ExitStatus{}, fmt.Errorf("%w: docker: env %q: DOCKER_ variables are the CLI's own", isolate.ErrInvalidCommand, k)
		}
	}
	name := i.workload
	dir := path.Join(isolate.WorkDir, c.Dir)
	if c.In == isolate.Connectors {
		if i.connectors == "" {
			return isolate.ExitStatus{}, fmt.Errorf("%w: the instance has no connectors compartment", isolate.ErrInvalidCommand)
		}
		name, dir = i.connectors, "/tmp"
	}
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return isolate.ExitStatus{}, isolate.ErrClosed
	}
	i.running.Add(1)
	i.mu.Unlock()
	defer i.running.Done()

	args := []string{"exec", "--workdir", dir}
	if stdio.Stdin != nil {
		args = append(args, "--interactive")
	}
	env := make([]string, 0, len(c.Env))
	for _, k := range sortedKeys(c.Env) {
		args = append(args, "--env", k)
		env = append(env, k+"="+c.Env[k])
	}
	args = append(append(args, name), c.Argv...)

	timeout := c.TimeoutOrDefault()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := i.d.command(runCtx, args...)
	cmd.Env = append(append([]string{}, i.d.env...), env...)
	cmd.Stdin = stdio.Stdin
	var mu sync.Mutex
	// The CLI reports a failed exec on stdout or stderr, depending on
	// the failure, so the end of both is kept to recognize it.
	outTail, errTail := &tailBuffer{max: 4096}, &tailBuffer{max: 4096}
	cmd.Stdout = &lockedWriter{mu: &mu, w: io.MultiWriter(orDiscard(stdio.Stdout), outTail)}
	cmd.Stderr = &lockedWriter{mu: &mu, w: io.MultiWriter(orDiscard(stdio.Stderr), errTail)}
	cmd.WaitDelay = 2 * time.Second

	runErr := cmd.Run()
	if i.isClosed() {
		return isolate.ExitStatus{}, isolate.ErrClosed
	}
	if runCtx.Err() != nil {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer rcancel()
		if _, err := i.d.cli(rctx, nil, "restart", "--time", "0", name); err != nil && !i.isClosed() {
			return isolate.ExitStatus{}, fmt.Errorf("isolate: docker: ending a stopped command: %w", err)
		}
		// A restarted gVisor sandbox is a new kernel, without the limit.
		if err := i.d.guestPIDs(rctx, name, i.pids[name]); err != nil && !i.isClosed() {
			return isolate.ExitStatus{}, fmt.Errorf("isolate: docker: after ending a stopped command: %w", err)
		}
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return isolate.ExitStatus{}, fmt.Errorf("%w: after %s", isolate.ErrTimeout, timeout)
		}
		return isolate.ExitStatus{}, fmt.Errorf("isolate: docker: %w", ctx.Err())
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return isolate.ExitStatus{}, fmt.Errorf("%w: docker exec: %w", isolate.ErrStart, runErr)
	}
	code := cmd.ProcessState.ExitCode()
	for _, tail := range []*tailBuffer{errTail, outTail} {
		if msg := tail.String(); code != 0 && cliFailed(code, msg) {
			return isolate.ExitStatus{}, fmt.Errorf("%w: docker exec: %s", isolate.ErrStart, lastLine(msg))
		}
	}
	return isolate.ExitStatus{Code: code}, nil
}

// cliFailed reports whether a docker exec exit came from the CLI or the
// runtime rather than the command: the command could not be started, or
// the daemon refused the exec.
func cliFailed(code int, stderr string) bool {
	switch code {
	case 126, 127:
		return strings.Contains(stderr, "OCI runtime exec failed")
	case 1:
		return strings.Contains(stderr, "Error response from daemon:")
	}
	return false
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func (i *instance) isClosed() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.closed
}

// Close implements isolate.Instance: it removes the containers and the
// socket volume, then the work directory.
func (i *instance) Close(ctx context.Context) error {
	i.once.Do(func() {
		i.mu.Lock()
		i.closed = true
		i.mu.Unlock()
		i.err = i.destroy(ctx)
		i.running.Wait()
		if i.err != nil {
			i.err = fmt.Errorf("isolate: docker: close %s: %w", i.id, i.err)
		}
	})
	return i.err
}

// destroy removes whatever start created. Parts that were never created
// are skipped.
func (i *instance) destroy(ctx context.Context) error {
	var errs []error
	names := []string{i.workload}
	if i.connectors != "" {
		names = append(names, i.connectors)
	}
	if _, err := i.d.cli(ctx, nil, append([]string{"rm", "--force", "--volumes"}, names...)...); err != nil && !strings.Contains(err.Error(), "No such container") {
		errs = append(errs, err)
	}
	for _, v := range []string{i.volume, i.workVolume} {
		if v == "" {
			continue
		}
		if _, err := i.d.cli(ctx, nil, "volume", "rm", "--force", v); err != nil {
			errs = append(errs, err)
		}
	}
	if i.local != nil {
		errs = append(errs, i.local.Close())
	}
	if i.work != "" {
		errs = append(errs, os.RemoveAll(i.work))
	}
	return errors.Join(errs...)
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// lockedWriter serializes writes through a mutex shared by a command's
// stdout and stderr, which may be the same writer.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
