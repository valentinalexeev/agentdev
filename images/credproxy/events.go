package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// Security events and usage lines go to stdout, one JSON line each. The operator follows the logs of the
// claude-proxy pods: the model never writes to them, so these lines are trusted (#66, closes R47).
//
//	AGENTDEV_SECURITY_EVENT {"source":"claude-proxy","rule":"...","severity":"...","agent":"...","action":"..."}
//	AGENTDEV_USAGE {"agent":"...","secret":"...","input_tokens":N,...}

const (
	eventPrefix = "AGENTDEV_SECURITY_EVENT "
	usagePrefix = "AGENTDEV_USAGE "
)

var (
	out   io.Writer = os.Stdout
	outMu sync.Mutex
)

type securityEvent struct {
	Source   string `json:"source"`
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Agent    string `json:"agent"`
	Action   string `json:"action"`
}

func emitLine(prefix string, v any) {
	b, _ := json.Marshal(v) // JSON escapes newlines, so one value is one line
	outMu.Lock()
	defer outMu.Unlock()
	fmt.Fprintf(out, "%s%s\n", prefix, b)
}

func emitEvent(agent, rule, severity, action string) {
	if len(action) > 500 {
		action = action[:500] + "..."
	}
	emitLine(eventPrefix, securityEvent{"claude-proxy", rule, severity, agent, action})
}
