package main

import (
	"bytes"
	"os"
	"path/filepath"
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

// #593: on SIGTERM the sidecar serves until the loop's loop-exit line, or the hold time
func TestWaitLoopExit(t *testing.T) {
	f := filepath.Join(t.TempDir(), "wake")
	if err := os.WriteFile(f, []byte("1 old loop-exit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if waitLoopExit(f, 300*time.Millisecond, 20*time.Millisecond) {
		t.Fatal("a loop-exit line from before the signal counts")
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		w, _ := os.OpenFile(f, os.O_APPEND|os.O_WRONLY, 0)
		_, _ = w.WriteString("2 loop-exit\n")
		_ = w.Close()
	}()
	start := time.Now()
	if !waitLoopExit(f, 5*time.Second, 20*time.Millisecond) || time.Since(start) > 2*time.Second {
		t.Fatal("did not return on the loop's line")
	}
	if termHold("x") != 25*time.Second || termHold("400") != 25*time.Second || termHold("0") != 0 || termHold("10") != 10*time.Second {
		t.Fatal("termHold")
	}
}
