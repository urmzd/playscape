package k8s

import (
	"os"
	"slices"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/scheme"
)

func decodeManifest(t *testing.T, path string) []runtime.Object {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := serializer.NewCodecFactory(scheme.Scheme, serializer.EnableStrict).UniversalDeserializer()
	var objs []runtime.Object
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		obj, _, err := dec.Decode([]byte(doc), nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		objs = append(objs, obj)
	}
	return objs
}

// TestRBACCoversExecutor keeps deploy/k8s/runner-rbac.yaml in step with the
// API calls Executor makes, and no broader.
func TestRBACCoversExecutor(t *testing.T) {
	want := map[string][]string{
		"batch/jobs": {"create", "delete", "get"},
		"/pods":      {"list"},
		"/pods/log":  {"get"},
	}
	got := map[string][]string{}
	for _, obj := range decodeManifest(t, "../../../deploy/k8s/runner-rbac.yaml") {
		role, ok := obj.(*rbacv1.Role)
		if !ok {
			continue
		}
		for _, r := range role.Rules {
			for _, g := range r.APIGroups {
				for _, res := range r.Resources {
					got[g+"/"+res] = append(got[g+"/"+res], r.Verbs...)
				}
			}
		}
	}
	if len(got) != len(want) {
		t.Fatalf("role resources = %v, want %v", got, want)
	}
	for k, verbs := range want {
		g := slices.Clone(got[k])
		slices.Sort(g)
		if !slices.Equal(g, verbs) {
			t.Fatalf("%s verbs = %v, want %v", k, g, verbs)
		}
	}
}

func TestNetworkPolicySelectsIsolatedRuns(t *testing.T) {
	objs := decodeManifest(t, "../../../deploy/k8s/runner-networkpolicy.yaml")
	np, ok := objs[0].(*networkingv1.NetworkPolicy)
	if !ok {
		t.Fatalf("got %T", objs[0])
	}
	if np.Spec.PodSelector.MatchLabels[IsolationLabel] != "isolated" || len(np.Spec.Ingress) != 0 || len(np.Spec.Egress) != 0 || len(np.Spec.PolicyTypes) != 2 {
		t.Fatalf("policy does not deny all for isolated runs: %+v", np.Spec)
	}
}
