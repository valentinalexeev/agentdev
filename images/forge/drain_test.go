package main

import (
	"bytes"
	"testing"
	"time"
)

func TestDrainedLine(t *testing.T) {
	var out bytes.Buffer
	old := eventOut
	eventOut = &out
	t.Cleanup(func() { eventOut = old })
	drainedGate.last = time.Time{}
	h := newServer(&Config{ForgeUser: "alice", AgentName: "alice"}).routes()
	if rec := call(t, h, "POST", "/drained", ""); rec.Code != 204 || out.String() != "AGENTDEV_DRAINED\n" {
		t.Fatalf("got %d %q", rec.Code, out.String())
	}
	call(t, h, "POST", "/drained", "")
	if out.String() != "AGENTDEV_DRAINED\n" {
		t.Fatalf("second report within a minute printed again: %q", out.String())
	}
	if rec := call(t, h, "GET", "/drained", ""); rec.Code == 204 {
		t.Fatal("GET accepted")
	}
}
