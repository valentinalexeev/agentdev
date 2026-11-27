package main

// Model and effort of an agent (#570): spec.model and spec.effort of the Agent object, set by an administrator (Ansible
// or Git), never through this API. They choose the Claude model and effort of the agent's runs and the default of its
// chat sessions, and the model decides how fast the agent spends its budget (claude-proxy multipliers).
//
// Like spec.permission (#505, permission.go), every change is one audit entry, action "model", written through the
// audit store's RecordChange (atomic across API processes; a change while the API was down is recorded once after the
// restart). An agent without an entry and with neither field set makes no entry; with a value it makes a "first seen"
// entry with before null. The values are no secret: the stdout line has them in full. Who made the change is not
// known to the API ("who" is empty); the Kubernetes audit log or the Git history names the person.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const modelAction = "model"

// agentClaude is spec.model and spec.effort; "" is Claude Code's default.
type agentClaude struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

func claudeOf(u *unstructured.Unstructured) *agentClaude {
	c := &agentClaude{}
	if u != nil {
		c.Model, _, _ = unstructured.NestedString(u.Object, "spec", "model")
		c.Effort, _, _ = unstructured.NestedString(u.Object, "spec", "effort")
	}
	return c
}

// modelEntry decides the "model" entry for cur, given the newest stored one (nil: none); nil when nothing is recorded.
func modelEntry(agent string, last []byte, cur *agentClaude) *auditEntry {
	e := &auditEntry{Time: now().Format(time.RFC3339), Action: modelAction, Agent: agent, Result: "ok",
		Reason: "spec.model or spec.effort of the Agent object changed", After: &agentState{Claude: cur}}
	var prev *agentClaude
	if last != nil {
		var l struct {
			After *agentState `json:"after"`
		}
		if json.Unmarshal(last, &l) == nil && l.After != nil {
			prev = l.After.Claude
		}
	}
	switch {
	case prev == nil && *cur == (agentClaude{}):
		return nil
	case prev == nil:
		e.Reason = firstSeenReason
	case *prev == *cur:
		return nil
	default:
		e.Before = &agentState{Claude: prev}
	}
	return e
}

// modelAudit feeds the Agents to the audit store; last is a per-process fast path only (as permissionAudit).
type modelAudit struct {
	mu   sync.Mutex
	last map[string]agentClaude
}

func (m *modelAudit) observe(ctx context.Context, u *unstructured.Unstructured) {
	name := u.GetName()
	cur := claudeOf(u)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		m.last = map[string]agentClaude{}
	}
	if prev, ok := m.last[name]; ok && prev == *cur {
		return
	}
	if auditDB == nil {
		m.last[name] = *cur
		return
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e, raw, err := auditDB.RecordChange(c, modelAction, name, func(last []byte) *auditEntry { return modelEntry(name, last, cur) })
	if err != nil {
		log.Printf("model audit %s: %v", name, err)
		return
	}
	if e != nil {
		fmt.Fprintf(auditOut, "AGENTDEV_AUDIT %s\n", raw)
	}
	m.last[name] = *cur
}

func (m *modelAudit) forget(name string) {
	m.mu.Lock()
	delete(m.last, name)
	m.mu.Unlock()
}
