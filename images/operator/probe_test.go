package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

func probeCatalogue() []v1.ExecutorProbe {
	return []v1.ExecutorProbe{
		{Name: "service-status", Description: "systemctl status of one unit", Command: []string{"systemctl", "status", "--no-pager", "{unit}"},
			Args: []v1.ExecutorProbeArg{{Name: "unit", Pattern: `[a-z0-9@-]+\.service`}}},
		{Name: "disk", Command: []string{"df", "-h"}},
		{Name: "journal", Sensitive: true, Command: []string{"journalctl", "-u", "{unit}", "-n", "50"},
			Args: []v1.ExecutorProbeArg{{Name: "unit", Pattern: `[a-z0-9@-]+\.service`}}},
	}
}

type probeCall struct{ keyID, command string }

func probeEnv(t *testing.T) (*execEnv, *ProbeServer, *[]probeCall) {
	x, _ := sshExecEnv(t, readyTarget("web1", "tf-prod", "web1.example.com", nil2key(t)))
	x.update(t, func(e *v1.Executor) { e.Spec.Probes = probeCatalogue() })
	s := newProbeServer(x.p, []byte(strings.Repeat("t", 40)), 600, 100)
	calls := &[]probeCall{}
	s.Run = func(_ context.Context, _ *SSHCA, tg *v1.SSHTarget, keyID, command string) (string, error) {
		*calls = append(*calls, probeCall{keyID, command})
		return "● nginx.service - active (running)\npassword=hunter2\n", nil
	}
	return x, s, calls
}

// #205 acceptance (operator side): a catalogue check runs and returns redacted output; an argument outside its pattern,
// an unknown check and option-like values are refused before any certificate is made, each a SecurityEvent.
func TestProbeCatalogue(t *testing.T) {
	x, s, calls := probeEnv(t)
	ctx := context.Background()
	a := s.Check(ctx, probeRequest{Target: "web1", Check: "service-status", Args: []string{"nginx.service"}})
	if a.Status != "ok" || !strings.Contains(a.Output, "active (running)") || strings.Contains(a.Output, "hunter2") {
		t.Fatalf("service-status: %+v", a)
	}
	if len(*calls) != 1 || (*calls)[0].command != "check service-status nginx.service" || (*calls)[0].keyID != "check/"+a.Run {
		t.Fatalf("ssh calls %+v", *calls)
	}
	r := x.run(t, a.Run)
	if r.Spec.Stage != "probe" || r.Spec.Action != "probe" || r.Spec.Target != "web1" || r.Spec.Check != "service-status" ||
		r.Status.Phase != "Succeeded" || r.Spec.Commit != "" || r.Spec.PR != 0 {
		t.Fatalf("probe record %+v %+v", r.Spec, r.Status)
	}
	// the poller leaves probe records alone (no Job)
	x.poll(t)
	var j batchv1.Job
	if err := x.c.Get(ctx, types.NamespacedName{Namespace: execNS, Name: a.Run}, &j); err == nil {
		t.Fatal("a probe run got a Job")
	}
	if x.run(t, a.Run).Status.Phase != "Succeeded" {
		t.Fatal("the poller changed a probe record")
	}

	for _, bad := range []probeRequest{
		{Target: "web1", Check: "service-status", Args: []string{"nginx"}},                  // pattern
		{Target: "web1", Check: "service-status", Args: []string{"nginx.service;id"}},       // charset
		{Target: "web1", Check: "service-status", Args: []string{"--help"}},                 // option
		{Target: "web1", Check: "service-status", Args: []string{"a.service", "b.service"}}, // count
		{Target: "web1", Check: "cat-shadow"},                                               // not in the catalogue
		{Target: "web1", Check: "service-status", Args: []string{"$(id).service"}},          // shell
		{Target: "web1", Check: "service-status", Args: []string{strings.Repeat("a", 130) + ".service"}},
	} {
		if a := s.Check(ctx, bad); a.Status != "refused" {
			t.Errorf("%+v: %+v", bad, a)
		}
	}
	if len(*calls) != 1 {
		t.Fatalf("a refused call reached the server: %+v", *calls)
	}
	refused, ran := 0, 0
	for _, ev := range x.events(t) {
		switch {
		case strings.HasPrefix(ev, "executor.probe-refused/medium"):
			refused++
		case strings.HasPrefix(ev, "executor.probe/low"):
			ran++
		}
	}
	// identical refusals at the same (test) instant share one event (hash of pod, rule, action and time)
	if refused < 4 || ran != 1 {
		t.Fatalf("events: %d refused, %d ran: %v", refused, ran, x.events(t))
	}
	// unknown or not-Ready target
	if a := s.Check(ctx, probeRequest{Target: "web9", Check: "disk"}); a.Status != "refused" {
		t.Fatalf("unknown target %+v", a)
	}
}

// #205 decision 2: a sensitive check needs an approval of exactly this call, used once.
func TestProbeSensitive(t *testing.T) {
	x, s, calls := probeEnv(t)
	ctx := context.Background()
	req := probeRequest{Target: "web1", Check: "journal", Args: []string{"nginx.service"}}
	a := s.Check(ctx, req)
	if a.Status != "pending" || len(*calls) != 0 || len(x.f.issues) != 1 {
		t.Fatalf("first call %+v, calls %d, issues %d", a, len(*calls), len(x.f.issues))
	}
	var l v1.ApprovalList
	_ = x.c.List(ctx, &l)
	if len(l.Items) != 1 || l.Items[0].Spec.Tool != "probe" || l.Items[0].Spec.Job != "issue-101" ||
		!strings.Contains(l.Items[0].Spec.Input, `argv ["journalctl","-u","nginx.service","-n","50"]`) {
		t.Fatalf("approval %+v", l.Items)
	}
	// still pending: no second request
	if a := s.Check(ctx, req); a.Status != "pending" {
		t.Fatalf("second call %+v", a)
	}
	_ = x.c.List(ctx, &l)
	if len(l.Items) != 1 {
		t.Fatalf("%d approvals", len(l.Items))
	}
	// another argument is another call: the approval does not cover it
	x.decide(t, l.Items[0].Name, "allowed", nil)
	other := probeRequest{Target: "web1", Check: "journal", Args: []string{"sshd.service"}}
	if a := s.Check(ctx, other); a.Status != "pending" || len(*calls) != 0 {
		t.Fatalf("approval reused for other args: %+v", a)
	}
	// the approved call runs once
	if a := s.Check(ctx, req); a.Status != "ok" || len(*calls) != 1 {
		t.Fatalf("approved call %+v", a)
	}
	if a := s.Check(ctx, req); a.Status != "pending" || len(*calls) != 1 {
		t.Fatalf("approval used twice: %+v", a)
	}
}

func TestProbeRateLimit(t *testing.T) {
	_, s, _ := probeEnv(t)
	s.global = newProbeServer(nil, nil, 1, 2).global
	n := 0
	for i := 0; i < 5; i++ {
		if a := s.Check(context.Background(), probeRequest{Target: "web1", Check: "disk"}); a.Status == "refused" && strings.HasPrefix(a.Message, "rate limit") {
			n++
		}
	}
	if n < 3 {
		t.Fatalf("rate limit refused %d of 5", n)
	}
}

func TestProbeHTTPAuth(t *testing.T) {
	_, s, _ := probeEnv(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	body, _ := json.Marshal(probeRequest{Target: "web1", Check: "disk"})
	for tok, want := range map[string]int{"": 401, "wrong": 401, strings.Repeat("t", 40): 200} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/check", bytes.NewReader(body))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("token %q: %d, want %d", tok, resp.StatusCode, want)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/catalogue", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 40))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var cat []catalogueEntry
	_ = json.NewDecoder(resp.Body).Decode(&cat)
	resp.Body.Close()
	if len(cat) != 3 || cat[0].Check != "disk" || !cat[1].Sensitive || cat[2].Args[0] != `unit=[a-z0-9@-]+\.service` {
		t.Fatalf("catalogue %+v", cat)
	}
}

func TestValidProbes(t *testing.T) {
	ok := ansibleExecutor()
	ok.Spec.Probes = probeCatalogue()
	if m := validExecutor(ok); m != "" {
		t.Fatalf("valid catalogue refused: %s", m)
	}
	for name, p := range map[string]v1.ExecutorProbe{
		"unused arg":     {Name: "a", Command: []string{"ls"}, Args: []v1.ExecutorProbeArg{{Name: "x", Pattern: "a"}}},
		"unknown hole":   {Name: "a", Command: []string{"ls", "{y}"}},
		"program hole":   {Name: "a", Command: []string{"{x}"}, Args: []v1.ExecutorProbeArg{{Name: "x", Pattern: "a"}}},
		"partial brace":  {Name: "a", Command: []string{"ls", "/{x}"}, Args: []v1.ExecutorProbeArg{{Name: "x", Pattern: "a"}}},
		"bad pattern":    {Name: "a", Command: []string{"ls", "{x}"}, Args: []v1.ExecutorProbeArg{{Name: "x", Pattern: "("}}},
		"shell metachar": {Name: "a", Command: []string{"sh", "-c", "id;id"}},
		"bad name":       {Name: "A", Command: []string{"ls"}},
	} {
		e := ansibleExecutor()
		e.Spec.Probes = []v1.ExecutorProbe{p}
		if validExecutor(e) == "" {
			t.Errorf("%s accepted", name)
		}
	}
	e := executor(nil)
	e.Spec.Probes = probeCatalogue()
	if validExecutor(e) == "" {
		t.Error("probes without ssh accepted")
	}
}

// the real SSH path: a fresh 5-minute certificate with the probe principal and force-command, as the check user,
// the host accepted only with a host certificate of the host CA.
func TestProbeSSHCertificate(t *testing.T) {
	f, _, ca := newFakeStepCA(t)
	srv := newTestSSHD(t, f.userCA.PublicKey())
	var issued *ssh.Certificate
	f.tamper = func(c *ssh.Certificate) {
		if c.CertType == ssh.UserCert {
			issued = c
		}
	}
	cert := &ssh.Certificate{Key: srv.hostKey.PublicKey(), CertType: ssh.HostCert, KeyId: "host/web1",
		ValidPrincipals: []string{"web1.example.com"}, ValidBefore: ssh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, f.hostCA); err != nil {
		t.Fatal(err)
	}
	srv.hostCert = cert
	tg := readyTarget("web1", "tf-prod", "web1.example.com", f.hostCA.PublicKey())
	s := &ProbeServer{P: &ExecutorPoller{Now: time.Now}, Dial: func(ctx context.Context, n, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, srv.addr)
	}}
	if _, err := s.sshProbeRun(context.Background(), ca, tg, "check/x-probe-1", "check disk"); err != nil {
		t.Fatal(err)
	}
	if srv.keyIDs[len(srv.keyIDs)-1] != "check/x-probe-1" || srv.commands[len(srv.commands)-1] != sshProbeCommand {
		t.Fatalf("server saw %v %v", srv.keyIDs, srv.commands)
	}
	if issued == nil || issued.CriticalOptions["force-command"] != sshProbeCommand ||
		strings.Join(issued.ValidPrincipals, ",") != sshProbePrincipal ||
		time.Duration(issued.ValidBefore-issued.ValidAfter)*time.Second > probeCertTTL+2*time.Minute {
		t.Fatalf("issued %+v", issued)
	}
	// another host CA: refused
	tg2 := readyTarget("web1", "tf-prod", "web1.example.com", nil2key(t))
	if _, err := s.sshProbeRun(context.Background(), ca, tg2, "check/x-probe-2", "check disk"); err == nil {
		t.Fatal("a server without a host certificate of the host CA was accepted")
	}
}
