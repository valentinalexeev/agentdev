package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeClaudeProxy answers GET /status like claude-proxy, with the job waiting or not.
func fakeClaudeProxy(t *testing.T, waiting *bool) string {
	t.Helper()
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			w.WriteHeader(404)
			return
		}
		_, _ = io.WriteString(w, `{"approvalStepUnits":5000,"jobs":[{"agent":"bob","job":"issue-3","units":1,"waitingApproval":true,"approvalStep":9},`+
			`{"agent":"alice","job":"issue-3","units":10100,"waitingApproval":`+map[bool]string{true: "true", false: "false"}[*waiting]+`,"approvalStep":2,"approvalLimit":10000}]}`)
	}))
	t.Cleanup(p.Close)
	return p.URL
}

func costCall(t *testing.T, h http.Handler, job string) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/cost-approval?job="+job, nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out struct{ State string }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		return "http-" + http.StatusText(rec.Code)
	}
	return out.State
}

// #310: the cost request carries the numbers claude-proxy reported (never the loop's), source cost, tool step-<n>.
func TestCostApprovalAllowed(t *testing.T) {
	s, fa, lines := approvalSetup(t)
	waiting := true
	s.appr.proxyURL = fakeClaudeProxy(t, &waiting)
	fa.state = "allowed"
	h := s.routes()
	if st := costCall(t, h, "issue-3"); st != "allowed" {
		t.Fatalf("state %s", st)
	}
	r := lastRequest(t, lines)
	if r.Source != "cost" || r.Tool != "step-2" || r.Job != "issue-3" ||
		!strings.Contains(r.Input, "used 10100 of 10000") || !strings.Contains(r.Input, "5000 more units, up to 15000") ||
		r.ContentHash != approvalHash(r.ID, "alice", "o/r", "issue-3", "cost", "step-2", r.Input) {
		t.Fatalf("request %+v", r)
	}
	if len(fa.comments) != 1 || !strings.Contains(fa.comments[0], "cost limit") || strings.Contains(fa.comments[0], "approve-job") {
		t.Errorf("comment %v", fa.comments)
	}
	// asked again for the same step (the proxy has not read the decision yet): no second request
	if st := costCall(t, h, "issue-3"); st != "allowed" || len(fa.comments) != 1 {
		t.Errorf("repeat: %s, %d comments", st, len(fa.comments))
	}
}

func TestCostApprovalDeniedAndNotWaiting(t *testing.T) {
	s, fa, _ := approvalSetup(t)
	waiting := false
	s.appr.proxyURL = fakeClaudeProxy(t, &waiting)
	h := s.routes()
	// the proxy does not report the job as waiting: nothing is filed, whatever the loop says
	if st := costCall(t, h, "issue-3"); st != "none" || len(fa.comments) != 0 {
		t.Fatalf("not waiting: %s %v", st, fa.comments)
	}
	waiting, fa.state = true, "denied"
	if st := costCall(t, h, "issue-3"); st != "denied" {
		t.Errorf("denied: %s", st)
	}
	if st := costCall(t, h, "issue-x"); st != "http-Bad Request" {
		t.Errorf("bad job: %s", st)
	}
	s.appr.proxyURL = ""
	if st := costCall(t, h, "issue-3"); st != "pending" {
		t.Errorf("no proxy: %s", st)
	}
}
