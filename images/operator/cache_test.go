package main

import (
	"context"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// #161: every kind the manager caches, in every namespace it caches it, must be listable and watchable under the Roles
// that manifests/operator.yaml.tmpl grants (the same template kind and GKE deploy). Otherwise the cache never syncs.
func TestCacheMatchesRoles(t *testing.T) {
	const agentsNS, execNS = "coding-agents", "agentdev-executors"
	docs := renderTmpl(t, "../../manifests/operator.yaml.tmpl", map[string]string{"AGENTS_NS": agentsNS, "EXEC_NS": execNS,
		"PLATFORM_NS": "agentdev-platform"})
	can := map[string]map[string]bool{} // namespace -> "group/resource" -> list and watch
	for _, d := range docs {
		var r rbacv1.Role
		if err := yaml.Unmarshal([]byte(d), &r); err != nil || r.Kind != "Role" || r.Name != "agentdev-operator" {
			continue
		}
		can[r.Namespace] = map[string]bool{}
		for _, rule := range r.Rules {
			if len(rule.ResourceNames) > 0 || !slices.Contains(rule.Verbs, "list") || !slices.Contains(rule.Verbs, "watch") {
				continue
			}
			for _, g := range rule.APIGroups {
				for _, res := range rule.Resources {
					can[r.Namespace][g+"/"+res] = true
				}
			}
		}
	}
	if len(can) != 2 {
		t.Fatalf("operator Roles found in %v, want %s and %s", can, agentsNS, execNS)
	}
	resource := func(o client.Object) string {
		n := strings.ToLower(reflect.TypeOf(o).Elem().Name())
		switch o.(type) {
		case *v1.Agent, *v1.Executor, *v1.ExecutorRun:
			return "agentdev.dev/" + n + "s"
		case *appsv1.StatefulSet:
			return "apps/statefulsets"
		case *corev1.PersistentVolumeClaim:
			return "/persistentvolumeclaims"
		}
		if strings.Contains(reflect.TypeOf(o).Elem().PkgPath(), "batch") {
			return "batch/" + n + "s"
		}
		return "/" + n + "s"
	}
	for _, execOn := range []string{"", execNS} {
		o := cacheOptions(agentsNS, execOn)
		nsOf := func(obj client.Object) []string {
			m := o.DefaultNamespaces
			for k, b := range o.ByObject {
				if reflect.TypeOf(k) == reflect.TypeOf(obj) && b.Namespaces != nil {
					m = b.Namespaces
				}
			}
			var out []string
			for n := range m {
				out = append(out, n)
			}
			return out
		}
		// the agent controller: For(Agent), Owns(StatefulSet), Owns(Service)
		kinds := []client.Object{&v1.Agent{}, &appsv1.StatefulSet{}, &corev1.Service{}}
		for k := range o.ByObject {
			kinds = append(kinds, k)
		}
		for _, k := range kinds {
			for _, n := range nsOf(k) {
				if !can[n][resource(k)] {
					t.Errorf("EXEC_NS=%q: %s cached in %s, but the Role there has no list/watch on %s", execOn,
						reflect.TypeOf(k).Elem().Name(), n, resource(k))
				}
			}
		}
		// the agent kinds stay in the agents namespace
		for _, k := range []client.Object{&v1.Agent{}, &appsv1.StatefulSet{}, &corev1.Service{}} {
			if got := nsOf(k); !reflect.DeepEqual(got, []string{agentsNS}) {
				t.Errorf("EXEC_NS=%q: %T cached in %v, want only %s", execOn, k, got, agentsNS)
			}
		}
		if execOn != "" {
			for _, k := range []client.Object{&v1.Executor{}, &v1.ExecutorRun{}} {
				if got := nsOf(k); !reflect.DeepEqual(got, []string{execNS}) {
					t.Errorf("%T cached in %v, want only %s", k, got, execNS)
				}
			}
		}
	}
}

type fakeSync struct {
	cache.Cache
	ok bool
}

func (f fakeSync) WaitForCacheSync(context.Context) bool { return f.ok }

// #161: readiness waits for the caches, so rollout status fails when a Role is missing a list/watch
func TestReadyzWaitsForCache(t *testing.T) {
	req := httptest.NewRequest("GET", "/readyz", nil)
	if cacheSynced(fakeSync{ok: false})(req) == nil {
		t.Error("ready before the caches synced")
	}
	if err := cacheSynced(fakeSync{ok: true})(req); err != nil {
		t.Error(err)
	}
}
