package main

// Permission mode of an agent (#505, the field is #500): spec.permission of the Agent object. Only an administrator
// sets it, in the Agent object (Ansible or Git), never through this API. The API shows it to the admin group in the
// agent detail and writes one audit entry (action "permission") each time it changes.
//
// The API watches the Agent objects. For each Agent it compares spec.permission with the last "permission" entry of
// that agent in the audit store, so a change while the API was down is still recorded once, after the restart, and a
// relist or restart without a change writes nothing. An agent without an entry counts as the default (dontAsk, no
// rules): an agent that never had another mode makes no entry. Who made the change is not known to the API: the entry
// has an empty "who"; the Kubernetes audit log or the Git history names the person.

import (
	"context"
	"encoding/json"
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

// permissionAudit compares the Agents with the last recorded permission and writes one entry per change.
type permissionAudit struct {
	mu   sync.Mutex
	last map[string]*agentPermission // per agent: the value of the last entry (or the default); filled lazily
}

// lastRecorded is the permission of the newest "permission" entry of the agent; the default when there is none.
func lastRecorded(ctx context.Context, agent string) (*agentPermission, error) {
	def := &agentPermission{Mode: "dontAsk"}
	if auditDB == nil {
		return def, nil
	}
	raw, err := auditDB.LastAudit(ctx, agent, permissionAction)
	if err != nil || raw == nil {
		return def, err
	}
	var e struct {
		After *agentState `json:"after"`
	}
	if json.Unmarshal(raw, &e) != nil || e.After == nil || e.After.Permission == nil {
		return def, nil
	}
	return e.After.Permission, nil
}

// observe handles one Agent (add or update). It never fails: a store error is logged and retried at the next event.
func (p *permissionAudit) observe(ctx context.Context, u *unstructured.Unstructured) {
	name := u.GetName()
	cur := permissionOf(u)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last == nil {
		p.last = map[string]*agentPermission{}
	}
	prev, ok := p.last[name]
	if !ok {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		var err error
		prev, err = lastRecorded(c, name)
		cancel()
		if err != nil {
			log.Printf("permission audit %s: read the last entry: %v", name, err)
			return
		}
	}
	if !samePermission(prev, cur) {
		audit(auditEntry{Action: permissionAction, Agent: name, Result: "ok", Reason: "spec.permission of the Agent object changed",
			Before: &agentState{Permission: prev}, After: &agentState{Permission: cur}})
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
	pa := &permissionAudit{}
	f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(d, 10*time.Minute, ns, nil)
	inf := f.ForResource(agentsGVR).Informer()
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				pa.observe(ctx, u)
			}
		},
		UpdateFunc: func(_, obj any) {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				pa.observe(ctx, u)
			}
		},
		DeleteFunc: func(obj any) {
			if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = t.Obj
			}
			if u, ok := obj.(*unstructured.Unstructured); ok {
				pa.forget(u.GetName())
			}
		},
	})
	f.Start(ctx.Done())
}
