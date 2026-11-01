package main

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func approvalLine(id, agent, input string) string {
	h := ApprovalHash(id, agent, "o/r", "issue-7", "claude", "Bash", input)
	return fmt.Sprintf(`2026-11-01T08:00:00Z AGENTDEV_APPROVAL_REQUEST {"id":%q,"repo":"o/r","job":"issue-7","source":"claude","tool":"Bash","input":%q,"contentHash":%q,"timeoutSeconds":600,"requesterId":42,"secretHash":"`+strings.Repeat("c", 64)+`"}`,
		id, input, h)
}

func TestApprovalFromLine(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-carol-0", Labels: map[string]string{"agent": "carol"}}}
	a, ok := approvalFromLine(approvalLine("0123456789abcdef", "carol", "npm install x"), pod, "forge", "ns")
	if !ok {
		t.Fatal("valid line refused")
	}
	if a.Name != "carol-0123456789abcdef" || a.Spec.Agent != "carol" || a.Spec.Forge != "gitea" || a.Spec.RequesterID != 42 ||
		a.Spec.Expires.Sub(a.Spec.Created.Time).Minutes() != 10 {
		t.Fatalf("unexpected approval: %+v", a)
	}
	cases := map[string]struct{ line, container string }{
		"agent container":   {approvalLine("0123456789abcdef", "carol", "x"), "agent"},
		"hash of other pod": {approvalLine("0123456789abcdef", "mallory", "x"), "forge"},
		"bad id":            {approvalLine("XYZ", "carol", "x"), "forge"},
		"tampered input":    {strings.Replace(approvalLine("0123456789abcdef", "carol", "x"), `"input":"x"`, `"input":"y"`, 1), "forge"},
		"no requester":      {strings.Replace(approvalLine("0123456789abcdef", "carol", "x"), `"requesterId":42`, `"requesterId":0`, 1), "forge"},
		"other line":        {"2026-11-01T08:00:00Z hello", "forge"},
		"no timestamp":      {"AGENTDEV_APPROVAL_REQUEST {}", "forge"},
	}
	for name, c := range cases {
		if _, ok := approvalFromLine(c.line, pod, c.container, "ns"); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}
