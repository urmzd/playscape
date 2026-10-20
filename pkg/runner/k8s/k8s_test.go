package k8s

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/urmzd/playscape/pkg/runner"
)

const ns = "runs"

func newExecutor(t *testing.T, client *fake.Clientset, mod func(*Config)) *Executor {
	t.Helper()
	cfg := Config{Namespace: ns, Image: "busybox:1.37", PollInterval: 5 * time.Millisecond}
	if mod != nil {
		mod(&cfg)
	}
	e, err := New(client, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// outcome is what the simulated Job controller does once a Job appears.
type outcome struct {
	container corev1.ContainerState // zero: no pod is created
	jobFailed string                // non-empty: Job Failed condition reason
	// conditions, when set, are the Job's conditions instead of jobFailed's.
	conditions []batchv1.JobCondition
}

// simulate plays the Job controller and kubelet: when the Job is created it
// creates its pod in the given container state and sets the Job condition.
func simulate(t *testing.T, client *fake.Clientset, o outcome) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for ctx.Err() == nil {
			list, err := client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
			if err != nil || len(list.Items) == 0 {
				time.Sleep(2 * time.Millisecond)
				continue
			}
			job := list.Items[0]
			if o.container != (corev1.ContainerState{}) {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-abcde", Namespace: ns, Labels: job.Spec.Template.Labels},
					Status: corev1.PodStatus{
						Phase:             corev1.PodPending,
						ContainerStatuses: []corev1.ContainerStatus{{Name: ContainerName, State: o.container}},
					},
				}
				if o.container.Terminated != nil {
					pod.Status.Phase = corev1.PodSucceeded
				}
				if _, err := client.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
					t.Errorf("create pod: %v", err)
				}
			}
			if o.jobFailed != "" || o.conditions != nil {
				job.Status.Conditions = o.conditions
				if o.conditions == nil {
					job.Status.Conditions = []batchv1.JobCondition{{
						Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: o.jobFailed, Message: "simulated",
					}}
				}
				if _, err := client.BatchV1().Jobs(ns).UpdateStatus(ctx, &job, metav1.UpdateOptions{}); err != nil {
					t.Errorf("update job: %v", err)
				}
			}
			return
		}
	}()
}

func exited(code int32) corev1.ContainerState {
	return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}}
}

func jobGone(t *testing.T, client *fake.Clientset, name string) {
	t.Helper()
	_, err := client.BatchV1().Jobs(ns).Get(context.Background(), name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("job %s not deleted: %v", name, err)
	}
}

func run(t *testing.T, e *Executor, req runner.Request) (runner.Result, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var logs bytes.Buffer
	res, err := e.Run(ctx, req, &logs)
	return res, logs.String(), err
}

func TestRunStreamsLogsAndExitCode(t *testing.T) {
	for _, code := range []int32{0, 3} {
		client := fake.NewClientset()
		e := newExecutor(t, client, nil)
		o := outcome{container: exited(code)}
		if code != 0 {
			o.jobFailed = "BackoffLimitExceeded" // backoffLimit 0: one failed pod fails the Job
		}
		simulate(t, client, o)
		res, logs, err := run(t, e, runner.Request{ID: "r1", Command: []string{"sh", "-c", "true"}})
		if err != nil {
			t.Fatalf("exit %d: %v", code, err)
		}
		if res.ExitCode != int(code) {
			t.Fatalf("exit code = %d, want %d", res.ExitCode, code)
		}
		if logs != "fake logs" { // the fake clientset's canned log body
			t.Fatalf("logs = %q", logs)
		}
		// A finished run is left for the TTL to reap.
		if _, err := client.BatchV1().Jobs(ns).Get(context.Background(), "playscape-run-r1", metav1.GetOptions{}); err != nil {
			t.Fatalf("finished job should remain for TTL cleanup: %v", err)
		}
	}
}

func TestRunImagePullFailureIsStartError(t *testing.T) {
	client := fake.NewClientset()
	e := newExecutor(t, client, nil)
	simulate(t, client, outcome{container: corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: "not found"},
	}})
	_, _, err := run(t, e, runner.Request{ID: "r2", Command: []string{"true"}})
	if !errors.Is(err, runner.ErrStart) {
		t.Fatalf("err = %v, want ErrStart", err)
	}
	jobGone(t, client, "playscape-run-r2")
}

func TestRunDeadlineIsTimeout(t *testing.T) {
	client := fake.NewClientset()
	e := newExecutor(t, client, nil)
	simulate(t, client, outcome{jobFailed: batchv1.JobReasonDeadlineExceeded})
	_, _, err := run(t, e, runner.Request{ID: "r3", Command: []string{"sleep", "999"}, Timeout: time.Second})
	if !errors.Is(err, runner.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	jobGone(t, client, "playscape-run-r3")
}

func TestRunDeadlineKillAfterStartIsTimeout(t *testing.T) {
	client := fake.NewClientset()
	e := newExecutor(t, client, nil)
	simulate(t, client, outcome{container: exited(137), jobFailed: batchv1.JobReasonDeadlineExceeded})
	_, _, err := run(t, e, runner.Request{ID: "r6", Command: []string{"sleep", "999"}})
	if !errors.Is(err, runner.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func TestRunCancelDeletesJob(t *testing.T) {
	client := fake.NewClientset()
	e := newExecutor(t, client, nil)
	simulate(t, client, outcome{container: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := e.Run(ctx, runner.Request{ID: "r4", Command: []string{"true"}}, &bytes.Buffer{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline", err)
	}
	jobGone(t, client, "playscape-run-r4")
}

func TestJobIsRestricted(t *testing.T) {
	e := newExecutor(t, fake.NewClientset(), func(c *Config) {
		c.NetworkIsolated = true
		c.TTL = 90 * time.Second
		c.Labels = map[string]string{"team": "a"}
	})
	job, err := e.Job(runner.Request{
		ID:        "r5",
		Command:   []string{"sh", "-c", "echo hi"},
		Env:       map[string]string{"A": "1"},
		Timeout:   1500 * time.Millisecond,
		Resources: runner.Resources{Memory: "64Mi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := job.Spec
	if *s.BackoffLimit != 0 || *s.ActiveDeadlineSeconds != 2 || *s.TTLSecondsAfterFinished != 90 {
		t.Fatalf("job spec = backoff %d deadline %d ttl %d", *s.BackoffLimit, *s.ActiveDeadlineSeconds, *s.TTLSecondsAfterFinished)
	}
	p := s.Template.Spec
	if p.RestartPolicy != corev1.RestartPolicyNever || *p.AutomountServiceAccountToken || *p.EnableServiceLinks || p.HostNetwork || p.HostPID || p.HostIPC {
		t.Fatalf("pod spec not restricted: %+v", p)
	}
	if !*p.SecurityContext.RunAsNonRoot || *p.SecurityContext.RunAsUser != DefaultUID ||
		p.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security context = %+v", p.SecurityContext)
	}
	c := p.Containers[0]
	sc := c.SecurityContext
	if *sc.Privileged || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || len(sc.Capabilities.Add) != 0 {
		t.Fatalf("container security context = %+v", sc)
	}
	if got := c.Resources.Limits.Memory().String(); got != "64Mi" {
		t.Fatalf("memory limit = %s", got)
	}
	if got := c.Resources.Limits.Cpu().String(); got != DefaultCPU {
		t.Fatalf("cpu limit = %s", got)
	}
	if c.Resources.Requests.Memory().Cmp(*c.Resources.Limits.Memory()) != 0 {
		t.Fatal("requests must equal limits")
	}
	if c.Resources.Limits.StorageEphemeral().IsZero() {
		t.Fatal("ephemeral storage unbounded")
	}
	for _, v := range p.Volumes {
		if v.EmptyDir == nil || v.EmptyDir.SizeLimit == nil {
			t.Fatalf("volume %s is not a size-limited emptyDir", v.Name)
		}
	}
	l := s.Template.Labels
	if l[RunLabel] != "r5" || l[IsolationLabel] != "isolated" || l["team"] != "a" || l[managedByLabel] != managedBy {
		t.Fatalf("labels = %v", l)
	}
	if len(c.Env) != 1 || c.Env[0].Name != "A" {
		t.Fatalf("env = %v", c.Env)
	}
}

func TestJobRejectsBadRequests(t *testing.T) {
	e := newExecutor(t, fake.NewClientset(), func(c *Config) { c.Image = "" })
	for name, req := range map[string]runner.Request{
		"no command":   {Image: "x"},
		"no image":     {Command: []string{"true"}},
		"bad quantity": {Image: "x", Command: []string{"true"}, Resources: runner.Resources{CPU: "lots"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := e.Job(req); !errors.Is(err, runner.ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestRunIDIsDNSSafe(t *testing.T) {
	if got := runID("ok-id-1"); got != "ok-id-1" {
		t.Fatalf("runID kept = %q", got)
	}
	for _, id := range []string{"", "Upper", "a_b", "-lead", "0123456789012345678901234567890123456789x"} {
		if got := runID(id); !dnsSafe.MatchString(got) || got == id {
			t.Fatalf("runID(%q) = %q", id, got)
		}
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(nil, Config{Namespace: ns}); err == nil {
		t.Fatal("nil client accepted")
	}
	if _, err := New(fake.NewClientset(), Config{}); err == nil {
		t.Fatal("empty namespace accepted")
	}
}

// kubeExpand applies Kubernetes' expansion of a container's command and args,
// as documented for define-command-argument-container: $$ becomes $, $(NAME)
// becomes NAME's value when the container's environment defines it, and
// anything else is left as it is.
func kubeExpand(s string, env map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$':
			b.WriteByte('$')
			i++
		case '(':
			end := strings.IndexByte(s[i+2:], ')')
			if end < 0 {
				b.WriteByte(s[i])
				continue
			}
			name := s[i+2 : i+2+end]
			if v, ok := env[name]; ok {
				b.WriteString(v)
			} else {
				b.WriteString(s[i : i+3+end])
			}
			i += 2 + end
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// The argv a request carries is the argv the process runs: the rendered Pod
// spec escapes every $ so Kubernetes' expansion gives the request's text back,
// whether it is a shell's $$, a $(NAME) the environment defines, or a lone $.
func TestCommandSurvivesKubernetesExpansion(t *testing.T) {
	e := newExecutor(t, fake.NewClientset(), nil)
	env := map[string]string{"HOME": "/work"}
	tests := []struct{ name, arg, rendered string }{
		{"shell pid", "echo $$", "echo $$$$"},
		{"a variable the container defines", "echo $(HOME)", "echo $$(HOME)"},
		{"a lone dollar", "costs $5 or $", "costs $$5 or $$"},
		{"no dollar", "echo hi", "echo hi"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job, err := e.Job(runner.Request{ID: "r7", Command: []string{"sh", "-c", tc.arg}, Env: env})
			if err != nil {
				t.Fatal(err)
			}
			cmd := job.Spec.Template.Spec.Containers[0].Command
			if cmd[2] != tc.rendered {
				t.Errorf("rendered %q, want %q", cmd[2], tc.rendered)
			}
			if got := kubeExpand(cmd[2], env); got != tc.arg {
				t.Errorf("Kubernetes would run %q, want %q", got, tc.arg)
			}
		})
	}
}

// A Job that hits its deadline is marked FailureTarget while its pod is
// terminated and Failed after, both with reason DeadlineExceeded. Whichever of
// them a run sees, alone or in either order, the run timed out, even when the
// killed container has already exited.
func TestRunDeadlineIsTimeoutWhicheverConditionIsSeen(t *testing.T) {
	target := batchv1.JobCondition{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded, Message: "simulated"}
	failedCond := batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded, Message: "simulated"}
	for name, conds := range map[string][]batchv1.JobCondition{
		"FailureTarget alone":        {target},
		"FailureTarget, then Failed": {target, failedCond},
		"Failed, then FailureTarget": {failedCond, target},
	} {
		for _, killed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, container exited %v", name, killed), func(t *testing.T) {
				client := fake.NewClientset()
				e := newExecutor(t, client, nil)
				o := outcome{conditions: conds}
				if killed {
					o.container = exited(137)
				}
				simulate(t, client, o)
				res, _, err := run(t, e, runner.Request{ID: "r8", Command: []string{"sleep", "999"}})
				if !errors.Is(err, runner.ErrTimeout) {
					t.Fatalf("deadline returned exit=%d err=%v, want ErrTimeout", res.ExitCode, err)
				}
			})
		}
	}
}

// A container that exits non-zero fails the Job too (backoffLimit is 0), and
// the two can land between the executor's look at the pod and its read of the
// Job. The run still reports the exit code: the Job's failure is the exit,
// not an error of its own. Here the first pod list is the moment both land:
// it finds no pod, and by the Job read the pod has exited and the Job failed.
func TestRunReportsTheExitCodeWhenTheJobFailsBetweenItsReads(t *testing.T) {
	client := fake.NewClientset()
	tracker := client.Tracker()
	jobsGVR := batchv1.SchemeGroupVersion.WithResource("jobs")
	podsGVR := corev1.SchemeGroupVersion.WithResource("pods")
	var once sync.Once
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		first := false
		once.Do(func() {
			first = true
			obj, err := tracker.Get(jobsGVR, ns, "playscape-run-r9")
			if err != nil {
				t.Errorf("get job: %v", err)
				return
			}
			job := obj.(*batchv1.Job)
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-abcde", Namespace: ns, Labels: job.Spec.Template.Labels},
				Status: corev1.PodStatus{
					Phase:             corev1.PodFailed,
					ContainerStatuses: []corev1.ContainerStatus{{Name: ContainerName, State: exited(3)}},
				},
			}
			if err := tracker.Create(podsGVR, pod, ns); err != nil {
				t.Errorf("create pod: %v", err)
			}
			job.Status.Conditions = []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "simulated",
			}}
			if err := tracker.Update(jobsGVR, job, ns); err != nil {
				t.Errorf("fail job: %v", err)
			}
		})
		if first {
			return true, &corev1.PodList{}, nil
		}
		return false, nil, nil
	})
	e := newExecutor(t, client, nil)
	res, _, err := run(t, e, runner.Request{ID: "r9", Command: []string{"sh", "-c", "exit 3"}})
	if err != nil {
		t.Fatalf("err = %v, want the exit code", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", res.ExitCode)
	}
}
