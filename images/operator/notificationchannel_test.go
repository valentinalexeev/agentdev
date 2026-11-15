package main

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// NotificationChannel (#347): the CRD validates the type, the VAPID reference and the host list.
func TestNotificationChannelCRD(t *testing.T) {
	b, err := os.ReadFile("config/crd/agentdev.dev_notificationchannels.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Scope    string `json:"scope"`
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties map[string]struct {
							Required   []string `json:"required"`
							Properties map[string]struct {
								Enum  []string `json:"enum"`
								Items struct {
									Pattern string `json:"pattern"`
								} `json:"items"`
							} `json:"properties"`
							XValidations []struct {
								Rule string `json:"rule"`
							} `json:"x-kubernetes-validations"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(b, &crd); err != nil {
		t.Fatal(err)
	}
	if crd.Spec.Scope != "Namespaced" || len(crd.Spec.Versions) != 1 {
		t.Fatalf("scope %q, %d versions", crd.Spec.Scope, len(crd.Spec.Versions))
	}
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if got := spec.Properties["type"].Enum; len(got) != 1 || got[0] != "webpush" {
		t.Fatalf("type enum %v", got)
	}
	if !strings.HasPrefix(spec.Properties["allowedHosts"].Items.Pattern, `^(\*\.)?`) {
		t.Fatalf("allowedHosts pattern %q", spec.Properties["allowedHosts"].Items.Pattern)
	}
	rules := ""
	for _, v := range spec.XValidations {
		rules += v.Rule + "\n"
	}
	for _, want := range []string{"has(self.vapid)", "startsWith('mailto:')"} {
		if !strings.Contains(rules, want) {
			t.Fatalf("missing rule %q in %s", want, rules)
		}
	}
}
