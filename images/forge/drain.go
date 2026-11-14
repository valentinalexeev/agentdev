package main

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// POST /drained (localhost API, #174): the loop was asked to drain (pod annotation agentdev/drain, set by the
// operator) and has no running job. The sidecar prints the line `AGENTDEV_DRAINED` on the forge container's stdout;
// the operator takes it only from this container (Claude runs in the agent container and can print anything there)
// and then replaces the pod with the new template. A tool Claude runs can call this too: the only effect is that its
// own pod is replaced sooner, as after the operator's maximum wait (docs/security.md R105). At most one line a minute.
const drainedLine = "AGENTDEV_DRAINED"

var drainedGate struct {
	mu   sync.Mutex
	last time.Time
}

func (s *server) drained(w http.ResponseWriter, _ *http.Request) {
	drainedGate.mu.Lock()
	now := time.Now()
	if now.Sub(drainedGate.last) < time.Minute {
		drainedGate.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	drainedGate.last = now
	drainedGate.mu.Unlock()
	eventMu.Lock()
	fmt.Fprintln(eventOut, drainedLine)
	eventMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
