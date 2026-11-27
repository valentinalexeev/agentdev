package main

// Label writes of a console chat session (#571, docs/adr/0011-chat-label-writes.md). The forge sidecar of a chat pod
// (CHAT_SESSION set, images/forge/chatlabels.go) adds labels to an issue or PR only when a person asked for exactly
// that call. A person asks by allowing, in the console, the Bash command that makes it: POST .../permissions/{pid}
// records who allowed (noteAllow). The sidecar then asks the internal listener:
//
//	POST /internal/chat/{id}/label-asked {"repo", "issue", "labels"} -> 200 {"person"} | 403
//
// The API answers only the pod of that session (caller address = the session pod's address), only for the session's
// repository, and only with an allow of the last chatAllowTTL whose Bash command, as the console showed it, names the
// labels path of that issue and every label. One allow is good for one label write. Each answer is an audit entry
// (action chat-label) with the person, or without one when refused. Allows live in memory: an API restart forgets
// them, and the sidecar then refuses (fail closed).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	chatAllowTTL      = 5 * time.Minute
	chatLabelMaxCount = 10
)

// chatAllow is one "allow" answer of a person in a session's console.
type chatAllow struct {
	pid  string
	who  auditWho
	name string // the author name the session server got
	at   time.Time
	used bool
}

// noteAllow records an allow before the session server gets it (the command can run at once); forget drops it again
// when the session server did not take the answer.
func (c *chatAPI) noteAllow(sid, pid string, who auditWho, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.nowFn()
	if c.allows == nil {
		c.allows = map[string][]*chatAllow{}
	}
	for s, l := range c.allows { // prune: an old allow never counts
		keep := l[:0]
		for _, a := range l {
			if now.Sub(a.at) < chatAllowTTL {
				keep = append(keep, a)
			}
		}
		if len(keep) == 0 {
			delete(c.allows, s)
		} else {
			c.allows[s] = keep
		}
	}
	c.allows[sid] = append(c.allows[sid], &chatAllow{pid: pid, who: who, name: name, at: now})
}

func (c *chatAPI) forgetAllow(sid, pid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.allows[sid]
	for i, a := range l {
		if a.pid == pid {
			c.allows[sid] = append(l[:i:i], l[i+1:]...)
			return
		}
	}
}

// chatLabelCommand reports whether a Bash command names the labels path of the issue and every label as a JSON
// string ("name", or \"name\" inside a quoted shell argument).
func chatLabelCommand(cmd, repo string, issue int64, labels []string) bool {
	if !strings.Contains(cmd, fmt.Sprintf("/repos/%s/issues/%d/labels", repo, issue)) {
		return false
	}
	for _, l := range labels {
		if !strings.Contains(cmd, `"`+l+`"`) && !strings.Contains(cmd, `\"`+l+`\"`) {
			return false
		}
	}
	return true
}

// permissionCommand gives the Bash command of permission request pid as the session's transcript has it (what the
// console showed the person). Other tools, and an input clipped by the session server, give "".
func (c *chatAPI) permissionCommand(ctx context.Context, sid, pid string) (string, error) {
	var after int64
	for {
		evs, err := c.store.Events(ctx, sid, after, 500)
		if err != nil {
			return "", err
		}
		for _, e := range evs {
			after = e.ID
			if e.Type != "permission" || e.Seq <= 0 {
				continue
			}
			var d struct {
				ID    string `json:"id"`
				Tool  string `json:"tool"`
				Input string `json:"input"`
			}
			if json.Unmarshal(e.Data, &d) != nil || d.ID != pid {
				continue
			}
			var in struct {
				Command string `json:"command"`
			}
			if d.Tool != "Bash" || json.Unmarshal([]byte(d.Input), &in) != nil {
				return "", nil
			}
			return in.Command, nil
		}
		if len(evs) < 500 {
			return "", nil
		}
	}
}

// matchAllow finds the newest unused allow of the session whose command names this label call, and uses it.
func (c *chatAPI) matchAllow(ctx context.Context, sid, repo string, issue int64, labels []string) (*chatAllow, error) {
	c.mu.Lock()
	now := c.nowFn()
	var cands []*chatAllow
	for _, a := range c.allows[sid] {
		if !a.used && now.Sub(a.at) < chatAllowTTL {
			cands = append(cands, a)
		}
	}
	c.mu.Unlock()
	for i := len(cands) - 1; i >= 0; i-- {
		a := cands[i]
		cmd, err := c.permissionCommand(ctx, sid, a.pid)
		if err != nil {
			return nil, err
		}
		if !chatLabelCommand(cmd, repo, issue, labels) {
			continue
		}
		c.mu.Lock()
		taken := a.used
		a.used = true
		c.mu.Unlock()
		if !taken {
			return a, nil
		}
	}
	return nil, nil
}

// labelAsked: POST /internal/chat/{id}/label-asked, from the forge sidecar of the session's pod.
func (c *chatAPI) labelAsked() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid := r.PathValue("id")
		if !chatIDRe.MatchString(sid) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such session"})
			return
		}
		var in struct {
			Repo   string   `json:"repo"`
			Issue  int64    `json:"issue"`
			Labels []string `json:"labels"`
		}
		dec := json.NewDecoder(io.LimitReader(r.Body, 8192))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil || !validRepo(in.Repo) || in.Issue <= 0 || in.Issue > 1e15 ||
			len(in.Labels) == 0 || len(in.Labels) > chatLabelMaxCount {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		for _, l := range in.Labels {
			if l == "" || len(l) > 200 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad label"})
				return
			}
		}
		s, err := c.store.GetSession(r.Context(), sid)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such session"})
			return
		}
		// only the session's own pod: its address now, as the cluster reports it
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		addr, err := c.cluster.SessionAddr(r.Context(), sid)
		pod, _, _ := net.SplitHostPort(addr)
		if err != nil || pod == "" || host != pod {
			log.Printf("chat %s: label-asked refused: caller is not the session's pod", sid)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		req := map[string]any{"session": sid, "repo": in.Repo, "issue": in.Issue, "labels": in.Labels}
		e := auditEntry{Action: "chat-label", Agent: s.Agent, Request: req}
		var a *chatAllow
		if s.EndedAt == nil && in.Repo == s.Repo {
			if a, err = c.matchAllow(r.Context(), sid, in.Repo, in.Issue, in.Labels); err != nil {
				log.Printf("chat %s: label-asked: %v", sid, err)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cannot read the transcript"})
				return
			}
		}
		if a == nil {
			e.Result, e.Reason = "denied", "no person allowed this label call in the console"
			audit(e)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": e.Reason})
			return
		}
		req["permission"] = a.pid
		e.Who, e.Result = a.who, "ok"
		audit(e)
		writeJSON(w, http.StatusOK, map[string]string{"person": a.name})
	})
}
