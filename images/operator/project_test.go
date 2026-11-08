package main

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

func project(name string, repos []string, text string) *v1.Project {
	return &v1.Project{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1.ProjectSpec{Repos: repos, GuideAppend: text}}
}

func agentCond(t *testing.T, r *AgentReconciler, name, typ string) *metav1.Condition {
	t.Helper()
	var a v1.Agent
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &a); err != nil {
		t.Fatal(err)
	}
	return meta.FindStatusCondition(a.Status.Conditions, typ)
}

// #54: the project's guideAppend comes after the fixed agentdev part, under its own heading, with the project repos
func TestProjectGuideAppended(t *testing.T) {
	p := project("shop", []string{"owner/repo", "owner/api"}, "Run `make check` before a PR.\r\nUse Go 1.26.")
	r, c := setup(t, p, agent("ana", func(a *v1.Agent) { a.Spec.Project = "shop" }), agent("bo", nil))
	reconcile(t, r, "ana")
	reconcile(t, r, "bo")
	g, _ := guideOf(t, c, "ana")
	fixed := strings.Index(g, "# agentdev harness guide")
	head := strings.Index(g, "## Project guide (from the members of project shop, not from agentdev)")
	if fixed != 0 || head < 0 || !strings.Contains(g[head:], "Run `make check` before a PR.\nUse Go 1.26.\n") {
		t.Fatalf("guide:\n%s", g)
	}
	if !strings.Contains(g, "- Project shop: repositories owner/repo owner/api.") {
		t.Errorf("project repos missing:\n%s", g)
	}
	if strings.Contains(g, "\r") {
		t.Error("carriage return kept")
	}
	if c := agentCond(t, r, "ana", "ProjectValid"); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("ProjectValid: %+v", c)
	}
	other, _ := guideOf(t, c, "bo")
	if strings.Contains(other, "Project guide") || agentCond(t, r, "bo", "ProjectValid") != nil {
		t.Error("an agent without a project got the project section or condition")
	}
}

// the project text cannot hide control sequences, and it is capped at a line end
func TestProjectGuideSanitizedAndCapped(t *testing.T) {
	long := strings.Repeat("line of project text\n", 1000) // > 8 KiB
	p := project("big", []string{"owner/repo"}, "a\x1b[2Jb\x00c\u0085d\n"+long)
	s := projectSection(p)
	if strings.ContainsAny(s, "\x1b\x00\u0085") || !strings.Contains(s, "a[2Jbcd\n") {
		t.Fatalf("control characters kept: %q", s[:80])
	}
	body := s[strings.Index(s, "\n\n")+2:]
	if len(body) > guideMaxProjectBytes+1 || !strings.HasSuffix(body, "text\n") {
		t.Fatalf("not capped at a line end: %d bytes, tail %q", len(body), body[len(body)-20:])
	}
	if strings.Contains(s, "- Project big: repositories") {
		t.Error("a one-repo project lists its repos")
	}
}

// a project that does not hold the agent's repo, or is on another forge, or is missing, is not used
func TestProjectNotUsedWhenItDoesNotMatch(t *testing.T) {
	cases := []struct {
		name string
		p    *v1.Project
		want string
	}{
		{"other-repo", project("p1", []string{"owner/api"}, "secret plan"), "is not in project"},
		{"other-forge", func() *v1.Project {
			p := project("p1", []string{"owner/repo"}, "secret plan")
			p.Spec.Forge = "github"
			return p
		}(), "is on forge github"},
		{"missing", project("unrelated", []string{"owner/repo"}, "secret plan"), "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, c := setup(t, tc.p, agent("cy", func(a *v1.Agent) { a.Spec.Project = "p1" }))
			reconcile(t, r, "cy")
			g, _ := guideOf(t, c, "cy")
			if strings.Contains(g, "secret plan") || strings.Contains(g, "Project guide") {
				t.Errorf("project text used:\n%s", g)
			}
			cond := agentCond(t, r, "cy", "ProjectValid")
			if cond == nil || cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, tc.want) {
				t.Errorf("ProjectValid: %+v", cond)
			}
			getSTS(t, c, "cy") // the agent still runs, without the project
		})
	}
}

// a Project change re-renders only the agents that name it
func TestProjectMapsToItsAgents(t *testing.T) {
	r, _ := setup(t, agent("di", func(a *v1.Agent) { a.Spec.Project = "shop" }), agent("ed", nil),
		agent("fa", func(a *v1.Agent) { a.Spec.Project = "other" }))
	reqs := r.agentsOfProject(context.Background(), project("shop", []string{"owner/repo"}, ""))
	if len(reqs) != 1 || reqs[0].Name != "di" {
		t.Fatalf("requests: %+v", reqs)
	}
}
