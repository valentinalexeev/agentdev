package main

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

// #251 part 3: every container of an agent pod has an ephemeral-storage limit, the mirror emptyDir a size limit
func TestAgentPodEphemeralStorageLimits(t *testing.T) {
	r, c := setup(t, agent("olga", nil))
	reconcile(t, r, "olga")
	spec := getSTS(t, c, "olga").Spec.Template.Spec
	for _, ct := range append(spec.InitContainers, spec.Containers...) {
		l, ok := ct.Resources.Limits["ephemeral-storage"]
		if !ok || l.IsZero() {
			t.Errorf("%s: no ephemeral-storage limit", ct.Name)
		}
		if q, ok := ct.Resources.Requests["ephemeral-storage"]; !ok || q.Cmp(l) > 0 {
			t.Errorf("%s: request %v above limit %v", ct.Name, q, l)
		}
	}
	for _, v := range spec.Volumes {
		if v.EmptyDir != nil && (v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.Cmp(resource.MustParse(defForgeMirrorLimit)) != 0) {
			t.Errorf("emptyDir %s: size limit %v", v.Name, v.EmptyDir.SizeLimit)
		}
	}
}

// the limits are settings (#265 review): configured values are used, empty or invalid ones give the defaults
func TestAgentPodEphemeralStorageConfigurable(t *testing.T) {
	r, c := setup(t, agent("olga", nil))
	r.D.AgentEphemeralRequest, r.D.AgentEphemeralLimit, r.D.ForgeMirrorLimit, r.D.ForgeEphemeralLimit = "2Gi", "20Gi", "bad", ""
	reconcile(t, r, "olga")
	spec := getSTS(t, c, "olga").Spec.Template.Spec
	want := map[string][2]string{"agent": {"2Gi", "20Gi"}, "forge": {"256Mi", defForgeEphemeralLimit}}
	for _, ct := range spec.Containers {
		w, ok := want[ct.Name]
		if !ok {
			continue
		}
		q, l := ct.Resources.Requests["ephemeral-storage"], ct.Resources.Limits["ephemeral-storage"]
		if q.String() != w[0] || l.String() != w[1] {
			t.Errorf("%s: request %s limit %s, want %v", ct.Name, q.String(), l.String(), w)
		}
	}
	for _, v := range spec.Volumes {
		if v.Name == "forge" && v.EmptyDir.SizeLimit.String() != defForgeMirrorLimit {
			t.Errorf("mirror limit %v, want the default for an invalid value", v.EmptyDir.SizeLimit)
		}
	}
}
