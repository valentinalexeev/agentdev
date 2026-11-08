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
		if v.EmptyDir != nil && (v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.Cmp(resource.MustParse(forgeMirrorLimit)) != 0) {
			t.Errorf("emptyDir %s: size limit %v", v.Name, v.EmptyDir.SizeLimit)
		}
	}
}
