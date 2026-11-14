package main

// Platform health (#344): GET /api/platform for the console's Platform page, agentdev-admins only (403 otherwise).
// Each component is read from the Kubernetes API (Deployment readiness, agent pods) or from a live forge call
// with the API's own read token. Nothing here reads or returns a Secret: the forge token is only used, never shown.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// component is one lamp on the Platform page.
type component struct {
	Name    string `json:"name"`    // api | router | operator | claude-proxy | authentik | forge-token | egress-proxy | agent-pool
	Status  string `json:"status"`  // ok | degraded | down | absent | unknown
	Detail  string `json:"detail"`  // one line for people, from trusted sources only
	Command string `json:"command"` // copyable command to look deeper
}

type platformView struct {
	Version    string      `json:"version"` // tag of the API image (the instance pins one release for all images)
	Checked    time.Time   `json:"checked"`
	Components []component `json:"components"`
}

// platformSource reads the platform state. kubeAgents implements it; tests use a fake.
type platformSource interface {
	// DeploymentReady returns ready and wanted replicas; absent=true when the Deployment does not exist.
	DeploymentReady(ctx context.Context, ns, name string) (ready, wanted int32, absent bool, err error)
	// AgentPodPhases counts the agent pods per phase (Pending, Running, ...).
	AgentPodPhases(ctx context.Context) (map[string]int, error)
}

// forgeProbe checks that the API's forge credential works (no value is returned).
type forgeProbe func(ctx context.Context) error

func (k *kubeAgents) DeploymentReady(ctx context.Context, ns, name string) (int32, int32, bool, error) {
	if ns == "" {
		ns = k.ns
	}
	d, err := k.c.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return 0, 0, true, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	return d.Status.ReadyReplicas, want, false, nil
}

func (k *kubeAgents) AgentPodPhases(ctx context.Context) (map[string]int, error) {
	l, err := k.c.CoreV1().Pods(k.ns).List(ctx, metav1.ListOptions{LabelSelector: "app=coding-agent"})
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, p := range l.Items {
		out[string(p.Status.Phase)]++
	}
	return out, nil
}

type platformChecker struct {
	src        platformSource
	forge      forgeProbe
	agentsNS   string
	platformNS string
	version    string
}

func imageTag(image string) string {
	if i := strings.LastIndex(image, "@"); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndex(image, ":"); i >= 0 && !strings.Contains(image[i:], "/") {
		return image[i+1:]
	}
	return ""
}

func newPlatformChecker(src platformSource, forge forgeProbe, agentsNS, platformNS string) *platformChecker {
	return &platformChecker{src: src, forge: forge, agentsNS: agentsNS, platformNS: platformNS, version: imageTag(os.Getenv("API_IMAGE"))}
}

func deployComponent(ctx context.Context, src platformSource, name, ns, deploy string, optional bool) component {
	c := component{Name: name, Command: fmt.Sprintf("kubectl -n %s describe deploy/%s", ns, deploy)}
	ready, want, absent, err := src.DeploymentReady(ctx, ns, deploy)
	switch {
	case err != nil:
		c.Status, c.Detail = "unknown", "cannot read the Deployment"
	case absent && optional:
		c.Status, c.Detail = "absent", "not installed"
	case absent:
		c.Status, c.Detail = "down", "Deployment not found"
	case want == 0:
		c.Status, c.Detail = "degraded", "scaled to 0"
	case ready >= want:
		c.Status, c.Detail = "ok", fmt.Sprintf("%d/%d ready", ready, want)
	case ready > 0:
		c.Status, c.Detail = "degraded", fmt.Sprintf("%d/%d ready", ready, want)
	default:
		c.Status, c.Detail = "down", fmt.Sprintf("0/%d ready", want)
	}
	return c
}

func (p *platformChecker) check(ctx context.Context) platformView {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	v := platformView{Version: p.version, Checked: time.Now().UTC()}
	comps := make([]component, 8)
	comps[0] = component{Name: "api", Status: "ok", Detail: "answering", Command: fmt.Sprintf("kubectl -n %s logs deploy/agentdev-api", p.platformNS)}
	var wg sync.WaitGroup
	run := func(i int, f func() component) {
		wg.Add(1)
		go func() { defer wg.Done(); comps[i] = f() }()
	}
	run(1, func() component { return deployComponent(ctx, p.src, "router", p.agentsNS, "webhook-router", false) })
	run(2, func() component {
		return deployComponent(ctx, p.src, "operator", p.platformNS, "agentdev-operator", false)
	})
	run(3, func() component { return deployComponent(ctx, p.src, "claude-proxy", p.agentsNS, "claude-proxy", true) })
	run(4, func() component {
		return deployComponent(ctx, p.src, "authentik", p.platformNS, "authentik-server", false)
	})
	run(5, func() component {
		c := component{Name: "forge-token", Command: fmt.Sprintf("kubectl -n %s logs deploy/agentdev-api | grep -i forge", p.platformNS)}
		switch err := p.forgeErr(ctx); {
		case err == nil:
			c.Status, c.Detail = "ok", "the API's forge credential works"
		case errors.Is(err, errNoForgeCredential):
			c.Status, c.Detail = "absent", "no forge credential configured: forge permission checks fail closed"
		default:
			c.Status, c.Detail = "down", "the forge refused or did not answer"
		}
		return c
	})
	run(6, func() component { return deployComponent(ctx, p.src, "egress-proxy", p.agentsNS, "egress-proxy", true) })
	run(7, func() component {
		c := component{Name: "agent-pool", Command: fmt.Sprintf("kubectl -n %s get pods -l app=coding-agent -o wide", p.agentsNS)}
		ph, err := p.src.AgentPodPhases(ctx)
		if err != nil {
			c.Status, c.Detail = "unknown", "cannot read the agent pods"
			return c
		}
		c.Detail = fmt.Sprintf("%d running, %d pending, %d failed", ph["Running"], ph["Pending"], ph["Failed"])
		switch {
		case ph["Failed"] > 0 || ph["Pending"] > 0:
			c.Status = "degraded"
		default:
			c.Status = "ok"
		}
		return c
	})
	wg.Wait()
	v.Components = comps
	return v
}

var errNoForgeCredential = errors.New("no forge credential")

func (p *platformChecker) forgeErr(ctx context.Context) error {
	if p.forge == nil {
		return errNoForgeCredential
	}
	return p.forge(ctx)
}

// forgeProbeFrom checks the Gitea token (GET /user) or, for GitHub, the App (GET /app) or token (GET /user).
func forgeProbeFrom(gp *giteaPerms, gh *githubForge) forgeProbe {
	hasGitea := gp != nil && gp.base != "" && gp.token != ""
	hasGitHub := gh != nil && (gh.static != "" || gh.key != nil)
	if !hasGitea && !hasGitHub {
		return nil
	}
	return func(ctx context.Context) error {
		if hasGitea {
			if _, err := gp.Self(ctx); err != nil {
				return err
			}
		}
		if hasGitHub {
			if _, err := gh.Self(ctx); err != nil { // static token: GET /user
				return err
			}
			if _, err := gh.AppBot(ctx); err != nil { // App: GET /app with the App JWT (cached for an hour)
				return err
			}
		}
		return nil
	}
}

func (p *platformChecker) handler(adminGroup string) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		id, _ := r.Context().Value(ctxIdentity{}).(*identity)
		w := &writer{adminGroup: adminGroup}
		if id == nil || !w.isAdmin(id) {
			writeJSON(rw, http.StatusForbidden, map[string]string{"error": "needs admin access"})
			return
		}
		writeJSON(rw, http.StatusOK, p.check(r.Context()))
	})
}
