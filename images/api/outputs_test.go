package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestChainLinksPerAgent(t *testing.T) {
	m := newMemStore()
	ctx := context.Background()
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 123456789, time.UTC)
	var alice []securityEvent
	for i, a := range []string{"alice", "bob", "alice", "alice"} {
		e := ev(a, "low", "h"+strconv.Itoa(i), t0.Add(time.Duration(i)*time.Second))
		if ok, _ := m.Insert(ctx, &e); !ok {
			t.Fatal("not inserted")
		}
		if a == "alice" {
			alice = append(alice, e)
		}
	}
	if alice[0].Prev != "" || alice[1].Prev != alice[0].Chain || alice[2].Prev != alice[1].Chain {
		t.Fatalf("not linked: %+v", alice)
	}
	if i, err := verifyChain("", alice); i != -1 || err != nil {
		t.Fatal(i, err)
	}
	// the chain survives a JSON round trip (what a SIEM receives)
	b, _ := json.Marshal(alice)
	var got []securityEvent
	_ = json.Unmarshal(b, &got)
	if i, err := verifyChain("", got); i != -1 {
		t.Fatal("after JSON:", err)
	}
	// gap: the SIEM misses the middle event
	if i, _ := verifyChain("", []securityEvent{alice[0], alice[2]}); i != 1 {
		t.Fatal("gap not found", i)
	}
	// edit: the action is changed
	ed := append([]securityEvent{}, alice...)
	ed[1].Action = "harmless"
	if i, err := verifyChain("", ed); i != 1 || !strings.Contains(err.Error(), "edited") {
		t.Fatal("edit not found", i, err)
	}
	// reorder
	if i, _ := verifyChain("", []securityEvent{alice[1], alice[0]}); i != 0 {
		t.Fatal("reorder not found")
	}
	// a verifier can start in the middle with the last chain value it kept
	if i, _ := verifyChain(alice[0].Chain, alice[1:]); i != -1 {
		t.Fatal("resume failed")
	}
}

func TestSyslogMessage(t *testing.T) {
	e := ev("alice", "high", "abc", time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC))
	e.ID, e.Action, e.Job = 9, `push "main"]\x`, "issue-3"
	link("", &e)
	b, err := syslogMessage("api-0", e)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "<106>1 2026-11-01T10:00:00.000000Z api-0 agentdev-api - security [agentdev@32473 schema=\"agentdev.security/v1\" id=\"9\" agent=\"alice\" job=\"issue-3\"") {
		t.Fatalf("header %q", s)
	}
	if !strings.Contains(s, `chain="`+e.Chain+`"]`) {
		t.Fatal("chain missing", s)
	}
	_, msg, _ := strings.Cut(s, "\ufeff")
	var back securityEvent
	if err := json.Unmarshal([]byte(msg), &back); err != nil || back.Action != e.Action || back.Chain != e.Chain {
		t.Fatalf("msg %q %v", msg, err)
	}
	if sdEscape(`a"b]c\`) != `a\"b\]c\\` {
		t.Fatal("escape")
	}
	// unknown severity: critical (facility 13 * 8 + 2)
	e.Severity = "weird"
	if b, _ := syslogMessage("", e); !strings.HasPrefix(string(b), "<106>1 ") || !strings.Contains(string(b), " - agentdev-api ") {
		t.Fatalf("%s", b)
	}
}

// selfSigned returns a server certificate for 127.0.0.1 and its PEM.
func selfSigned(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "siem"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true,
		BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(k)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	cert, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}))
	if err != nil {
		t.Fatal(err)
	}
	return cert, string(certPEM)
}

func TestSyslogOverTLS(t *testing.T) {
	cert, caPEM := selfSigned(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 2)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		r := bufio.NewReader(c)
		for i := 0; i < 2; i++ { // octet-counted frames: "<len> <msg>"
			ls, err := r.ReadString(' ')
			if err != nil {
				return
			}
			n, _ := strconv.Atoi(strings.TrimSpace(ls))
			buf := make([]byte, n)
			if _, err := io.ReadFull(r, buf); err != nil {
				return
			}
			got <- string(buf)
		}
	}()
	env := map[string]string{"SECURITY_SYSLOG_ADDR": ln.Addr().String(), "SECURITY_SYSLOG_CA": strings.ReplaceAll(caPEM, "\n", `\n`)}
	outs, err := outputsFromEnv(func(k string) string { return env[k] })
	if err != nil || len(outs) != 1 || outs[0].Name != "syslog" {
		t.Fatal(outs, err)
	}
	for _, h := range []string{"h1", "h2"} {
		if err := outs[0].deliver(context.Background(), ev("alice", "low", h, time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []string{"h1", "h2"} {
		select {
		case m := <-got:
			if !strings.HasPrefix(m, "<109>1 ") || !strings.Contains(m, `hash="`+h+`"`) {
				t.Fatalf("frame %q", m)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no frame")
		}
	}
	// without the CA the self-signed collector is refused: certificates are always verified
	delete(env, "SECURITY_SYSLOG_CA")
	outs, _ = outputsFromEnv(func(k string) string { return env[k] })
	outs[0].Attempts = 1
	if err := outs[0].deliver(context.Background(), ev("a", "low", "x", time.Now())); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
}

func TestOTLPOutput(t *testing.T) {
	var body []byte
	var hdr http.Header
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		hdr = r.Header.Clone()
		w.WriteHeader(200)
	}))
	defer srv.Close()
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	env := map[string]string{"SECURITY_OTLP_URL": srv.URL + "/v1/logs", "SECURITY_OTLP_CA": caPEM,
		"SECURITY_OTLP_HEADERS": "Authorization=Bearer%20tok, x-team=sec"}
	outs, err := outputsFromEnv(func(k string) string { return env[k] })
	if err != nil || len(outs) != 1 {
		t.Fatal(err)
	}
	e := ev("alice", "medium", "h1", time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC))
	e.ID = 4
	link("", &e)
	if err := outs[0].deliver(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if hdr.Get("Authorization") != "Bearer tok" || hdr.Get("X-Team") != "sec" || hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", hdr)
	}
	var req struct {
		ResourceLogs []struct {
			ScopeLogs []struct {
				LogRecords []struct {
					TimeUnixNano   string                       `json:"timeUnixNano"`
					SeverityNumber int                          `json:"severityNumber"`
					Body           struct{ StringValue string } `json:"body"`
					Attributes     []otlpAttr                   `json:"attributes"`
				} `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	rec := req.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if rec.SeverityNumber != 13 || rec.TimeUnixNano != "1793527200000000000" {
		t.Fatalf("%+v", rec)
	}
	attrs := map[string]any{}
	for _, a := range rec.Attributes {
		for _, v := range a.Value {
			attrs[a.Key] = v
		}
	}
	if attrs["event.id"] != "h1" || attrs["agentdev.chain"] != e.Chain || attrs["agentdev.id"] != "4" || attrs["user.name"] != "alice" {
		t.Fatalf("attrs %v", attrs)
	}
	var full securityEvent
	if err := json.Unmarshal([]byte(rec.Body.StringValue), &full); err != nil || full.Chain != e.Chain {
		t.Fatal("body", err)
	}
}

func TestOutputsConfigRefusesInsecure(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unsigned webhook": {"SECURITY_WEBHOOK_URL": "https://x"},
		"plain otlp":       {"SECURITY_OTLP_URL": "http://collector:4318/v1/logs"},
		"bad syslog addr":  {"SECURITY_SYSLOG_ADDR": "collector"},
		"bad CA":           {"SECURITY_SYSLOG_ADDR": "c:6514", "SECURITY_SYSLOG_CA": "nope"},
		"half mTLS":        {"SECURITY_OTLP_URL": "https://c/v1/logs", "SECURITY_OTLP_CLIENT_CERT": "x"},
		"bad headers":      {"SECURITY_OTLP_URL": "https://c/v1/logs", "SECURITY_OTLP_HEADERS": "novalue"},
	} {
		if _, err := outputsFromEnv(func(k string) string { return env[k] }); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	outs, err := outputsFromEnv(func(string) string { return "" })
	if err != nil || len(outs) != 0 {
		t.Fatal("empty config", outs, err)
	}
}

func TestSinkFansOut(t *testing.T) {
	a, b := newWebhook("", nil), newWebhook("", nil)
	s := &sink{store: newMemStore(), outs: []*webhook{a, b}}
	s.add(context.Background(), ev("alice", "low", "h", time.Now()))
	ea, oka := a.next()
	eb, okb := b.next()
	if !oka || !okb || ea.Chain == "" || ea.Chain != eb.Chain {
		t.Fatal("not sent to every output with its chain")
	}
}
