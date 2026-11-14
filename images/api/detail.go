package main

// Agent detail (#344): GET /api/agents/{name} for the agent drawer of the console (docs/console-ux.md §7, §11).
// Visible to the admin group and to people with write access on one of the agent's repositories (the same rule as
// security events, R35); everyone else gets 404, so the endpoint does not tell which agents exist.

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const guidesCM = "agentdev-guides" // written by the operator (images/operator/guide.go), one key per agent

type podView struct {
	Name     string    `json:"name"`
	Phase    string    `json:"phase"` // Pending | Running | Succeeded | Failed | Unknown
	Ready    bool      `json:"ready"`
	Restarts int32     `json:"restarts"`
	Started  time.Time `json:"started,omitzero"`
	Reason   string    `json:"reason,omitempty"` // waiting reason of a container, e.g. CrashLoopBackOff
}

// waitingItem is one thing the agent waits on: a person's decision, a usage pause, a pause by a person, a block.
type waitingItem struct {
	Kind   string    `json:"kind"` // approval | usage-pause | paused | blocked | invalid
	Ref    string    `json:"ref,omitempty"`
	Job    string    `json:"job,omitempty"`
	Repo   string    `json:"repo,omitempty"`
	Reason string    `json:"reason,omitempty"`
	Since  time.Time `json:"since,omitzero"`
	Until  time.Time `json:"until,omitzero"`
}

type agentDetail struct {
	agent
	Pods       []podView     `json:"pods"`
	WaitingOn  []waitingItem `json:"waitingOn"`
	RecentJobs []job         `json:"recentJobs"`      // newest first, at most 10 (GET /api/jobs, #345); [] until then
	Guide      string        `json:"guide,omitempty"` // harness guide, operator-written (trusted)
}

// detailSource reads what the drawer needs beyond the agent list. kubeAgents implements it.
type detailSource interface {
	AgentPods(ctx context.Context, name string) ([]podView, error)
	Guide(ctx context.Context, name string) (string, error)
}

func (k *kubeAgents) AgentPods(ctx context.Context, name string) ([]podView, error) {
	l, err := k.c.CoreV1().Pods(k.ns).List(ctx, metav1.ListOptions{LabelSelector: "app=coding-agent,agent=" + name})
	if err != nil {
		return nil, err
	}
	out := []podView{}
	for _, p := range l.Items {
		v := podView{Name: p.Name, Phase: string(p.Status.Phase)}
		if p.Status.StartTime != nil {
			v.Started = p.Status.StartTime.Time.UTC()
		}
		v.Ready = len(p.Status.ContainerStatuses) > 0
		for _, c := range p.Status.ContainerStatuses {
			v.Restarts += c.RestartCount
			v.Ready = v.Ready && c.Ready
			if c.State.Waiting != nil && v.Reason == "" {
				v.Reason = c.State.Waiting.Reason
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (k *kubeAgents) Guide(ctx context.Context, name string) (string, error) {
	cm, err := k.c.CoreV1().ConfigMaps(k.ns).Get(ctx, guidesCM, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return cm.Data[name], nil
}

// canSeeAgent: admin group, or write access on one of the agent's repositories (fail closed on forge errors).
func (w *writer) canSeeAgent(ctx context.Context, id *identity, a agent) bool {
	if id == nil {
		return false
	}
	if w.isAdmin(id) {
		return true
	}
	for _, repo := range a.Repos {
		if p, err := w.repoPerm(ctx, id, a.Forge, repo); err == nil && p >= permWrite {
			return true
		}
	}
	return false
}

func (w *writer) agentDetail(rw http.ResponseWriter, r *http.Request) {
	id, _ := r.Context().Value(ctxIdentity{}).(*identity)
	name := r.PathValue("name")
	if len(name) > 63 || !agentNameRe.MatchString(name) {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "bad name"})
		return
	}
	lister, ok := w.store.(agentLister)
	if !ok {
		writeJSON(rw, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	list, err := lister.List(r.Context())
	if err != nil {
		log.Printf("agent detail: list: %v", err)
		writeJSON(rw, http.StatusBadGateway, map[string]string{"error": "cannot read agents"})
		return
	}
	var a *agent
	for i := range list {
		if list[i].Name == name {
			a = &list[i]
		}
	}
	if a == nil || !w.canSeeAgent(r.Context(), id, *a) {
		writeJSON(rw, http.StatusNotFound, map[string]string{"error": "not found"}) // do not reveal other projects
		return
	}
	one := []agent{*a}
	addBudgets(r.Context(), one)
	d := agentDetail{agent: one[0], Pods: []podView{}, WaitingOn: []waitingItem{}, RecentJobs: []job{}}
	if w.events != nil {
		if c, err := w.events.Counts(r.Context(), time.Now().Add(-7*24*time.Hour)); err == nil {
			d.SecurityEvents = c[name]
		}
	}
	if ds, ok := w.store.(detailSource); ok {
		if p, err := ds.AgentPods(r.Context(), name); err == nil {
			d.Pods = p
		} else {
			log.Printf("agent detail %s: pods: %v", name, err)
		}
		if g, err := ds.Guide(r.Context(), name); err == nil {
			d.Guide = g
		} else {
			log.Printf("agent detail %s: guide: %v", name, err)
		}
	}
	d.WaitingOn = w.waitingOn(r.Context(), *a)
	d.RecentJobs = w.recentJobs(r.Context(), id, *a)
	writeJSON(rw, http.StatusOK, d)
}

// waitingOn collects what blocks the agent now, from trusted sources only (Agent spec and status, Approval objects,
// the operator's usage pauses). Never from agent output.
func (w *writer) waitingOn(ctx context.Context, a agent) []waitingItem {
	out := []waitingItem{}
	switch {
	case a.Paused:
		out = append(out, waitingItem{Kind: "paused", Reason: "paused by a person"})
	case a.Phase == "blocked":
		out = append(out, waitingItem{Kind: "blocked"})
	case a.Phase == "invalid":
		out = append(out, waitingItem{Kind: "invalid"})
	}
	if as, ok := w.store.(approvalStore); ok {
		if objs, err := as.ListApprovals(ctx); err == nil {
			for _, u := range objs {
				ap := fromApprovalObject(u)
				if ap.Agent == a.Name && ap.State == "pending" {
					out = append(out, waitingItem{Kind: "approval", Ref: ap.Name, Job: ap.Job, Repo: ap.Repo,
						Reason: ap.Tool, Since: ap.Created, Until: ap.Expires})
				}
			}
		}
	}
	if w.usage != nil {
		if st, err := w.usage.State(ctx); err == nil {
			var pauses map[string]struct {
				Until  int64  `json:"until"`
				Reason string `json:"reason"`
			}
			if json.Unmarshal(st["pauses"], &pauses) == nil {
				if p, ok := pauses[a.Name]; ok && p.Until > time.Now().Unix() {
					out = append(out, waitingItem{Kind: "usage-pause", Reason: p.Reason, Until: time.Unix(p.Until, 0).UTC()})
				}
			}
		}
	}
	return out
}
