package main

// Permission mode of an agent (#505, the field is #500): spec.permission of the Agent object. Only an administrator
// sets it, in the Agent object (Ansible or Git), never through this API. The API shows it to the admin group in the
// agent detail and writes one audit entry (action "permission") each time it changes.
//
// The API watches the Agent objects. For each Agent the audit store compares spec.permission with the last
// "permission" entry of that agent and inserts in one transaction (RecordPermission), so a change while the API was
// down is still recorded once, after the restart, and a relist, a restart or a second API process writes nothing more.
// An agent without an entry and with the default (dontAsk, no rules) makes no entry; with another value it makes a
// "first seen" entry with before null. The stdout line has the mode, rule counts and a sha256 of the rules only; the
// rule text is only in api_audit (admin group). Who made the change is not known to the API: the entry
// has an empty "who"; the Kubernetes audit log or the Git history names the person.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

const permissionAction = "permission"

// agentPermission is spec.permission with the defaults applied: Mode is never empty.
type agentPermission struct {
	Mode     string         `json:"mode"` // dontAsk | auto
	AutoMode *agentAutoMode `json:"autoMode,omitempty"`
}

// agentAutoMode holds the classifier rules; the lists are never null in JSON, so the console can show "none".
type agentAutoMode struct {
	Allow       []string `json:"allow"`
	SoftDeny    []string `json:"softDeny"`
	Environment []string `json:"environment"`
}

// permissionOf reads spec.permission. A missing field or mode is dontAsk (the CRD default).
func permissionOf(u *unstructured.Unstructured) *agentPermission {
	p := &agentPermission{Mode: "dontAsk"}
	if u == nil {
		return p
	}
	if m, _, _ := unstructured.NestedString(u.Object, "spec", "permission", "mode"); m != "" {
		p.Mode = m
	}
	if _, ok, _ := unstructured.NestedMap(u.Object, "spec", "permission", "autoMode"); ok {
		list := func(f string) []string {
			v, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "permission", "autoMode", f)
			if v == nil {
				v = []string{}
			}
			return v
		}
		p.AutoMode = &agentAutoMode{Allow: list("allow"), SoftDeny: list("softDeny"), Environment: list("environment")}
	}
	return p
}

func samePermission(a, b *agentPermission) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Mode != b.Mode || (a.AutoMode == nil) != (b.AutoMode == nil) {
		return false
	}
	if a.AutoMode == nil {
		return true
	}
	return slices.Equal(a.AutoMode.Allow, b.AutoMode.Allow) && slices.Equal(a.AutoMode.SoftDeny, b.AutoMode.SoftDeny) &&
		slices.Equal(a.AutoMode.Environment, b.AutoMode.Environment)
}

// permissionAudit feeds the Agents to the audit store; last is a per-process fast path only.
type permissionAudit struct {
	mu   sync.Mutex
	last map[string]*agentPermission // per agent: the value seen at the last event of this process
}

const firstSeenReason = "first seen (no earlier entry)"

// permissionEntry decides the entry for cur, given the newest stored "permission" entry (nil: none). It returns nil
// when nothing is to be recorded: cur equals the last recorded value, or there is no earlier entry and cur is the
// default. With no earlier entry and a value other than the default (a new agent, or after a prune of the audit
// table or a restart of the in-memory store) the old value is not known: before is null, reason "first seen".
func permissionEntry(agent string, last []byte, cur *agentPermission) *auditEntry {
	e := &auditEntry{Time: now().Format(time.RFC3339), Action: permissionAction, Agent: agent, Result: "ok",
		Reason: "spec.permission of the Agent object changed", After: &agentState{Permission: cur}}
	var prev *agentPermission
	if last != nil {
		var l struct {
			After *agentState `json:"after"`
		}
		if json.Unmarshal(last, &l) == nil && l.After != nil {
			prev = l.After.Permission
		}
	}
	switch {
	case prev == nil && samePermission(cur, &agentPermission{Mode: "dontAsk"}):
		return nil
	case prev == nil:
		e.Reason = firstSeenReason
	case samePermission(prev, cur):
		return nil
	default:
		e.Before = &agentState{Permission: prev}
	}
	return e
}

// permissionSummary is what the stdout line shows of a permission: the mode, the rule counts and a sha256 of the
// rules, never the rule text (the cluster log pipeline has a wider audience than api_audit).
func permissionSummary(st *agentState) any {
	if st == nil || st.Permission == nil {
		return nil
	}
	p := st.Permission
	out := map[string]any{"mode": p.Mode}
	if p.AutoMode != nil {
		b, _ := json.Marshal(p.AutoMode)
		sum := sha256.Sum256(b)
		out["rules"] = map[string]any{"allow": len(p.AutoMode.Allow), "softDeny": len(p.AutoMode.SoftDeny),
			"environment": len(p.AutoMode.Environment), "sha256": hex.EncodeToString(sum[:])}
	}
	return map[string]any{"permission": out}
}

// printPermission writes the AGENTDEV_AUDIT line of a stored "permission" entry, with the rules summarised.
func printPermission(e *auditEntry) {
	b, _ := json.Marshal(e)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	m["before"], m["after"] = permissionSummary(e.Before), permissionSummary(e.After)
	b, _ = json.Marshal(m)
	fmt.Fprintf(auditOut, "AGENTDEV_AUDIT %s\n", b)
}

// observe handles one Agent (add or update). The cache only skips the store when the value did not change since the
// last event in this process; the store decides (atomically, across API processes) whether an entry is written. A
// store error is logged and retried at the next event.
func (p *permissionAudit) observe(ctx context.Context, u *unstructured.Unstructured) {
	name := u.GetName()
	cur := permissionOf(u)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last == nil {
		p.last = map[string]*agentPermission{}
	}
	if prev, ok := p.last[name]; ok && samePermission(prev, cur) {
		return
	}
	if auditDB == nil {
		p.last[name] = cur
		return
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e, _, err := auditDB.RecordPermission(c, name, cur)
	if err != nil {
		log.Printf("permission audit %s: %v", name, err)
		return
	}
	if e != nil {
		printPermission(e)
	}
	p.last[name] = cur
}

// forget drops a deleted Agent from the cache; the audit store keeps its entries.
func (p *permissionAudit) forget(name string) {
	p.mu.Lock()
	delete(p.last, name)
	p.mu.Unlock()
}

// watchPermissions follows the Agent objects in ns (an informer, like watchSecurityEvents).
func watchPermissions(ctx context.Context, d dynamic.Interface, ns string) {
	pa, ma := &permissionAudit{}, &modelAudit{} // #570: spec.model and spec.effort, the same way
	f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(d, 10*time.Minute, ns, nil)
	inf := f.ForResource(agentsGVR).Informer()
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				pa.observe(ctx, u)
				ma.observe(ctx, u)
			}
		},
		UpdateFunc: func(_, obj any) {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				pa.observe(ctx, u)
				ma.observe(ctx, u)
			}
		},
		DeleteFunc: func(obj any) {
			if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = t.Obj
			}
			if u, ok := obj.(*unstructured.Unstructured); ok {
				pa.forget(u.GetName())
				ma.forget(u.GetName())
			}
		},
	})
	f.Start(ctx.Done())
}
