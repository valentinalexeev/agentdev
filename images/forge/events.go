package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"
)

// Security events (#64, owner request; collected by #70): every refusal prints one line to stdout,
//
//	AGENTDEV_SECURITY_EVENT {"source":"forge","rule":"...","severity":"low|medium|high","agent":"...","job":"...","action":"..."}
//
// so a bypass attempt is reported, not only blocked. The job comes from the loop's X-Agentdev-Job header (issue-N /
// pr-N); it is informational only, Claude could set it when calling the sidecar directly.

const eventPrefix = "AGENTDEV_SECURITY_EVENT "

var (
	eventOut io.Writer = os.Stdout
	eventMu  sync.Mutex
	jobRe    = regexp.MustCompile(`^(issue|pr)-[0-9]{1,10}$`)
)

type securityEvent struct {
	Source   string `json:"source"`
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Agent    string `json:"agent"`
	Job      string `json:"job,omitempty"`
	Action   string `json:"action"`
}

func emitEvent(agent, job, rule, severity, action string) {
	if !jobRe.MatchString(job) {
		job = ""
	}
	if len(action) > 500 {
		action = action[:500] + "..."
	}
	b, _ := json.Marshal(securityEvent{"forge", rule, severity, agent, job, action}) // JSON escapes newlines
	eventMu.Lock()
	defer eventMu.Unlock()
	fmt.Fprintf(eventOut, "%s%s\n", eventPrefix, b)
}
