// Package k8s is a runner.Executor that runs each request as one
// Kubernetes batch/v1 Job: a single non-restarting pod under a restricted
// security context (non-root, no privilege escalation, all capabilities
// dropped, read-only root filesystem, RuntimeDefault seccomp), CPU, memory,
// and scratch-disk limits, no service-account token, an active deadline,
// and TTL cleanup. Logs are streamed from the pod and the exit code is read
// from its terminated container state.
//
// Orthogonality boundary: this package owns the Job lifecycle only. It
// needs RBAC to create Jobs and read pods and logs in one namespace (see
// deploy/k8s/runner-rbac.yaml); network isolation is an optional
// NetworkPolicy keyed on the IsolationLabel the executor sets.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/urmzd/playscape/pkg/runner"
	"github.com/urmzd/playscape/pkg/task"
)

const (
	// RunLabel carries the run ID on the Job and its pod.
	RunLabel = "playscape/run"
	// IsolationLabel is set to "isolated" when Config.NetworkIsolated is
	// true; deploy/k8s/runner-networkpolicy.yaml selects on it.
	IsolationLabel = "playscape/network"
	// ContainerName is the run container's name inside the pod.
	ContainerName = "run"

	managedByLabel = "app.kubernetes.io/managed-by"
	managedBy      = "playscape"
	workDir        = "/work"
	namePrefix     = "playscape-run-"
	backstop       = 2 * time.Minute
)

// Defaults applied when Config fields are zero.
const (
	DefaultTTL          = 10 * time.Minute
	DefaultPollInterval = time.Second
	DefaultCPU          = "500m"
	DefaultMemory       = "256Mi"
	DefaultScratch      = "256Mi"
	DefaultUID          = 65532 // distroless "nonroot"
)

// Config configures an Executor.
type Config struct {
	// Namespace receives the Jobs. Required.
	Namespace string
	// Image is used when a Request has none. Required unless every
	// request names an image.
	Image string
	// ImagePullPolicy defaults to the cluster's behavior when empty.
	ImagePullPolicy corev1.PullPolicy
	// Resources are default limits; a Request's non-empty fields win.
	// Requests are set equal to limits.
	Resources runner.Resources
	// Timeout is the default run timeout; zero means runner.DefaultTimeout.
	// It becomes the Job's activeDeadlineSeconds, which counts scheduling
	// and image pull too.
	Timeout time.Duration
	// TTL is how long a finished Job is kept before the cluster deletes
	// it; zero means DefaultTTL.
	TTL time.Duration
	// NetworkIsolated labels run pods so an operator NetworkPolicy can
	// deny them network access.
	NetworkIsolated bool
	// RunAsUser is the non-root UID; zero means DefaultUID.
	RunAsUser int64
	// Labels are added to every Job and pod.
	Labels map[string]string
	// PollInterval paces status checks; zero means DefaultPollInterval.
	PollInterval time.Duration
}

// Executor runs requests as Kubernetes Jobs. It is safe for concurrent use.
type Executor struct {
	client kubernetes.Interface
	cfg    Config
}

var _ runner.Executor = (*Executor)(nil)

// New returns an Executor that creates Jobs through client.
func New(client kubernetes.Interface, cfg Config) (*Executor, error) {
	if client == nil {
		return nil, errors.New("k8s: nil client")
	}
	if cfg.Namespace == "" {
		return nil, errors.New("k8s: namespace required")
	}
	if cfg.TTL == 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = runner.DefaultTimeout
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.RunAsUser == 0 {
		cfg.RunAsUser = DefaultUID
	}
	if cfg.Resources.CPU == "" {
		cfg.Resources.CPU = DefaultCPU
	}
	if cfg.Resources.Memory == "" {
		cfg.Resources.Memory = DefaultMemory
	}
	if cfg.Resources.EphemeralStorage == "" {
		cfg.Resources.EphemeralStorage = DefaultScratch
	}
	if cfg.TTL < 0 || cfg.Timeout < 0 || cfg.PollInterval < 0 || cfg.RunAsUser < 0 {
		return nil, errors.New("k8s: negative duration or uid")
	}
	return &Executor{client: client, cfg: cfg}, nil
}

// Run implements runner.Executor: create the Job, wait for its pod, stream
// its logs to logs, and read the exit code. When ctx ends or the run
// cannot start, the Job is deleted; otherwise the TTL reaps it.
func (e *Executor) Run(ctx context.Context, req runner.Request, logs io.Writer) (runner.Result, error) {
	job, err := e.Job(req)
	if err != nil {
		return runner.Result{}, err
	}
	jobs := e.client.BatchV1().Jobs(e.cfg.Namespace)
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return runner.Result{}, fmt.Errorf("%w: create job %s: %w", runner.ErrStart, job.Name, err)
	}

	// The Job's active deadline is authoritative; the local backstop only
	// guarantees Run returns if the cluster never reports back.
	deadline := time.Duration(*job.Spec.ActiveDeadlineSeconds)*time.Second + backstop
	runCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	res, err := e.follow(runCtx, job, logs)
	if err != nil && ctx.Err() == nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%w: job %s: no result within %s", runner.ErrTimeout, job.Name, deadline)
	}
	if err != nil {
		e.cleanup(job.Name)
	}
	return res, err
}

func (e *Executor) follow(ctx context.Context, job *batchv1.Job, logs io.Writer) (runner.Result, error) {
	pod, err := e.waitPod(ctx, job, func(p *corev1.Pod) bool {
		return p.Status.Phase == corev1.PodRunning || terminated(p) != nil
	})
	if err != nil {
		return runner.Result{}, err
	}

	stream, err := e.client.CoreV1().Pods(e.cfg.Namespace).
		GetLogs(pod.Name, &corev1.PodLogOptions{Container: ContainerName, Follow: true}).
		Stream(ctx)
	if err != nil {
		return runner.Result{}, fmt.Errorf("k8s: logs %s: %w", pod.Name, err)
	}
	_, copyErr := io.Copy(logs, stream)
	stream.Close()
	if ctx.Err() != nil {
		return runner.Result{}, fmt.Errorf("k8s: run %s: %w", job.Name, ctx.Err())
	}
	if copyErr != nil {
		return runner.Result{}, fmt.Errorf("k8s: logs %s: %w", pod.Name, copyErr)
	}

	pod, err = e.waitPod(ctx, job, func(p *corev1.Pod) bool { return terminated(p) != nil })
	if err != nil {
		return runner.Result{}, err
	}
	// A deadline kill also terminates the container; report it as a
	// timeout, not as the signal's exit code.
	if j, err := e.client.BatchV1().Jobs(e.cfg.Namespace).Get(ctx, job.Name, metav1.GetOptions{}); err == nil {
		if c := failed(j); c != nil && c.Reason == batchv1.JobReasonDeadlineExceeded {
			return runner.Result{}, fmt.Errorf("%w: job %s: %s", runner.ErrTimeout, job.Name, c.Message)
		}
	}
	return runner.Result{ExitCode: int(terminated(pod).ExitCode)}, nil
}

// waitPod polls until the Job's pod satisfies done, the Job fails, or the
// pod cannot start.
func (e *Executor) waitPod(ctx context.Context, job *batchv1.Job, done func(*corev1.Pod) bool) (*corev1.Pod, error) {
	pods := e.client.CoreV1().Pods(e.cfg.Namespace)
	jobs := e.client.BatchV1().Jobs(e.cfg.Namespace)
	selector := RunLabel + "=" + job.Labels[RunLabel]
	tick := time.NewTicker(e.cfg.PollInterval)
	defer tick.Stop()
	// pod reports the Job's pod when it satisfies done; a pod that cannot
	// start is an error.
	pod := func() (*corev1.Pod, error) {
		list, err := pods.List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil && ctx.Err() == nil {
			return nil, fmt.Errorf("k8s: list pods %s: %w", selector, err)
		}
		if err == nil && len(list.Items) > 0 {
			p := &list.Items[0]
			if done(p) {
				return p, nil
			}
			if reason := startFailure(p); reason != "" {
				return nil, fmt.Errorf("%w: pod %s: %s", runner.ErrStart, p.Name, reason)
			}
		}
		return nil, nil
	}
	for {
		if p, err := pod(); p != nil || err != nil {
			return p, err
		}
		if j, err := jobs.Get(ctx, job.Name, metav1.GetOptions{}); err == nil {
			if c := failed(j); c != nil {
				if c.Reason == batchv1.JobReasonDeadlineExceeded {
					return nil, fmt.Errorf("%w: job %s: %s", runner.ErrTimeout, job.Name, c.Message)
				}
				// The pod may have finished between the list above and
				// this read: a container that exits non-zero fails the Job
				// (backoffLimit is 0), and its exit code is the result, not
				// the Job's failure. Look once more before reporting it.
				if p, err := pod(); p != nil || err != nil {
					return p, err
				}
				return nil, fmt.Errorf("k8s: job %s failed: %s: %s", job.Name, c.Reason, c.Message)
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("k8s: run %s: %w", job.Name, ctx.Err())
		case <-tick.C:
		}
	}
}

// cleanup deletes the Job and its pod, detached from the caller's context
// so cancellation still tears the run down.
func (e *Executor) cleanup(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bg := metav1.DeletePropagationBackground
	_ = e.client.BatchV1().Jobs(e.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &bg})
}

// Job renders the batch/v1 Job for req without creating it. It is exported
// so operators can inspect or dry-run what the executor submits.
func (e *Executor) Job(req runner.Request) (*batchv1.Job, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	image := req.Image
	if image == "" {
		image = e.cfg.Image
	}
	if image == "" {
		return nil, fmt.Errorf("%w: no image", runner.ErrInvalidRequest)
	}
	id := runID(req.ID)
	timeout := req.Timeout
	if timeout == 0 {
		timeout = e.cfg.Timeout
	}
	limits, err := e.limits(req.Resources)
	if err != nil {
		return nil, err
	}

	labels := map[string]string{}
	for k, v := range e.cfg.Labels {
		labels[k] = v
	}
	labels[managedByLabel] = managedBy
	labels[RunLabel] = id
	if e.cfg.NetworkIsolated {
		labels[IsolationLabel] = "isolated"
	}

	env := make([]corev1.EnvVar, 0, len(req.Env))
	for k, v := range req.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}

	scratch := limits[corev1.ResourceEphemeralStorage]
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: namePrefix + id, Namespace: e.cfg.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr(int32(0)),
			ActiveDeadlineSeconds:   ptr(int64(math.Ceil(timeout.Seconds()))),
			TTLSecondsAfterFinished: ptr(int32(math.Ceil(e.cfg.TTL.Seconds()))),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr(false),
					EnableServiceLinks:           ptr(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr(true),
						RunAsUser:      ptr(e.cfg.RunAsUser),
						RunAsGroup:     ptr(e.cfg.RunAsUser),
						FSGroup:        ptr(e.cfg.RunAsUser),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:            ContainerName,
						Image:           image,
						ImagePullPolicy: e.cfg.ImagePullPolicy,
						Command:         escapeExpansion(req.Command),
						Env:             env,
						WorkingDir:      workDir,
						Resources:       corev1.ResourceRequirements{Limits: limits, Requests: limits},
						SecurityContext: &corev1.SecurityContext{
							Privileged:               ptr(false),
							AllowPrivilegeEscalation: ptr(false),
							ReadOnlyRootFilesystem:   ptr(true),
							RunAsNonRoot:             ptr(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "work", MountPath: workDir},
							{Name: "tmp", MountPath: "/tmp"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &scratch}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &scratch}}},
					},
				},
			},
		},
	}, nil
}

func (e *Executor) limits(r runner.Resources) (corev1.ResourceList, error) {
	pick := func(req, def string) string {
		if req != "" {
			return req
		}
		return def
	}
	out := corev1.ResourceList{}
	for name, q := range map[corev1.ResourceName]string{
		corev1.ResourceCPU:              pick(r.CPU, e.cfg.Resources.CPU),
		corev1.ResourceMemory:           pick(r.Memory, e.cfg.Resources.Memory),
		corev1.ResourceEphemeralStorage: pick(r.EphemeralStorage, e.cfg.Resources.EphemeralStorage),
	} {
		v, err := resource.ParseQuantity(q)
		if err != nil {
			return nil, fmt.Errorf("%w: %s %q: %w", runner.ErrInvalidRequest, name, q, err)
		}
		out[name] = v
	}
	return out, nil
}

// dnsSafe matches IDs usable verbatim in a Job name and label value.
var dnsSafe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// runID returns id if it is DNS-safe and short enough for a Job name,
// otherwise a fresh random ID.
func runID(id string) string {
	if dnsSafe.MatchString(id) {
		return id
	}
	return task.NewID()[:16]
}

func terminated(p *corev1.Pod) *corev1.ContainerStateTerminated {
	for _, s := range p.Status.ContainerStatuses {
		if s.Name == ContainerName && s.State.Terminated != nil {
			return s.State.Terminated
		}
	}
	return nil
}

// startFailure reports why a pod can never start, or "" if it still might.
func startFailure(p *corev1.Pod) string {
	for _, s := range p.Status.ContainerStatuses {
		if w := s.State.Waiting; w != nil {
			switch w.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName",
				"CreateContainerConfigError", "CreateContainerError":
				return strings.TrimSpace(w.Reason + ": " + w.Message)
			}
		}
	}
	return ""
}

// failed returns the condition saying the Job has failed: Failed, or the
// FailureTarget the controller sets first, while it terminates the pods, with
// the same reason. A deadline kill is visible as FailureTarget before it is
// as Failed, so reading only Failed reports the killed container's exit code
// instead of a timeout.
func failed(j *batchv1.Job) *batchv1.JobCondition {
	var target *batchv1.JobCondition
	for i, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobFailed:
			return &j.Status.Conditions[i]
		case batchv1.JobFailureTarget:
			if target == nil {
				target = &j.Status.Conditions[i]
			}
		}
	}
	return target
}

// escapeExpansion protects argv from Kubernetes' $(VAR) expansion of a
// container's command and args. Kubernetes replaces $(NAME) with a variable
// from the container's environment and $$ with $, so argv passed through
// unchanged is not the argv that runs: a shell's "echo $$" would print "$"
// instead of a process id, and "$(HOME)" would be replaced before the shell
// saw it. Doubling every $ makes Kubernetes hand the process exactly the
// argv the request carried.
func escapeExpansion(argv []string) []string {
	if argv == nil {
		return nil
	}
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = strings.ReplaceAll(a, "$", "$$")
	}
	return out
}

func ptr[T any](v T) *T { return &v }
