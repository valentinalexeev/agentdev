package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"regexp"
	"strings"
	"sync"
)

// Auto mode classifier allows (#506, option A). With `--permission-mode auto` (#500) Claude Code asks a model whether
// a tool call outside the allow rules may run. That request goes through this proxy like every model request, and
// it is the only place outside the agent container that sees an allow: hooks and the approval MCP server see only
// blocks. Every allow becomes one low-severity security event `classifier-allow` naming the agent (from the proxy
// credential), the tool and its input.
//
// Shape (verified against Claude Code 2.1.328, the version in the agent image):
//   - The request is a POST /v1/messages whose system prompt has a block starting with
//     classifierMarker (a main-loop request starts its blocks with the attribution header or "You are Claude Code"). `querySource` ("auto_mode") is
//     internal to Claude Code and not sent; the `auto-mode-classifier-*` beta header is sent only to first-party URLs,
//     so neither is visible here.
//   - The user content is `<transcript>\n`, the earlier tool calls as JSON lines, the action under review as the last
//     JSON line (`{"Bash":"rm -rf x"}`, optionally with an "id" key), `</transcript>\n`, then an instruction suffix.
//     Claude Code escapes transcript tags inside the content, and JSON escapes newlines, so the last line before the
//     last `</transcript>` is the action.
//   - The reply holds `<block>yes` (block) or `<block>no` (allow), after an optional `<thinking>` part; stage 1 stops at
//     the `</block>` stop sequence. Claude Code reads a reply with both verdicts as no verdict, and so does this file.
//     In severity mode the reply is `<severity>N</severity>` instead: every score is a `classifier-severity` event.
//     Any other reply format is logged once and gives no event until it is supported.
//
// Trust (docs/security.md R121): the agent cannot suppress an allow, because Claude Code makes the request itself,
// not the model. The agent holds its proxy key (R60), so it can send a look-alike request and create false events,
// but only for its own agent: the agent name comes from the credential, never from the body.

const (
	classifierMarker   = "You are a security monitor for autonomous AI coding agents"
	classifierBodyCap  = 4 << 20 // classifier transcripts are bounded; a larger request is passed through unread
	classifierReplyCap = 256 << 10
	classifierInputMax = 300
)

type classifierCall struct {
	tool  string
	input string
}

// logOnce logs an unknown classifier format once per kind, so a format change does not flood the log.
var classifierLogged sync.Map

func classifierLogOnce(kind, msg string) {
	if _, seen := classifierLogged.LoadOrStore(kind, true); !seen {
		log.Printf("classifier detector: %s (logged once; the request is passed through unchanged)", msg)
	}
}

type msgContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// texts reads a string or an array of content blocks; other block types are skipped.
func texts(raw json.RawMessage) []string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}
	}
	var blocks []msgContent
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var out []string
	for _, b := range blocks {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return out
}

// detectClassifier returns the call under review for a classifier request, nil for any other request. A request
// that looks like a classifier request but whose action cannot be read is logged once and gives nil.
func detectClassifier(body []byte) *classifierCall {
	var req struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil
	}
	isClassifier := false
	for _, t := range texts(req.System) { // the prompt starts a system block; text inside a block does not count
		if strings.HasPrefix(strings.TrimSpace(t), classifierMarker) {
			isClassifier = true
		}
	}
	if !isClassifier {
		return nil
	}
	var user strings.Builder
	for _, m := range req.Messages {
		if m.Role == "user" {
			for _, t := range texts(m.Content) {
				user.WriteString(t)
			}
		}
	}
	call, ok := classifierAction(user.String())
	if !ok {
		classifierLogOnce("request", "classifier request without a readable action")
		return nil
	}
	return call
}

// classifierAction reads the last JSON line before the last `</transcript>`: one key is the tool, its value the
// input; an "id" key is ignored.
func classifierAction(s string) (*classifierCall, bool) {
	end := strings.LastIndex(s, "</transcript>")
	if end < 0 {
		return nil, false
	}
	head := strings.TrimRight(s[:end], "\r\n\t ")
	line := head[strings.LastIndexAny(head, "\n")+1:]
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &obj) != nil {
		return nil, false
	}
	delete(obj, "id")
	if len(obj) != 1 {
		return nil, false
	}
	for k, v := range obj {
		var in string
		if json.Unmarshal(v, &in) != nil {
			in = string(v) // structured input: keep the JSON
		}
		return &classifierCall{tool: k, input: in}, true
	}
	return nil, false
}

var (
	thinkingRe     = regexp.MustCompile(`<thinking>[\s\S]*?</thinking>`)
	thinkingTailRe = regexp.MustCompile(`<thinking>[\s\S]*$`)
	blockRe        = regexp.MustCompile(`(?i)<block>(yes|no)\b`)
)

// classifierVerdict mirrors Claude Code's reading of the reply: "allow", "block" or "" (no verdict).
func classifierVerdict(text string) string {
	seen := map[string]bool{}
	for _, m := range blockRe.FindAllStringSubmatch(text, -1) {
		seen[strings.ToLower(m[1])] = true
	}
	if len(seen) > 1 {
		return ""
	}
	seen = map[string]bool{}
	for _, m := range blockRe.FindAllStringSubmatch(thinkingTailRe.ReplaceAllString(thinkingRe.ReplaceAllString(text, ""), ""), -1) {
		seen[strings.ToLower(m[1])] = true
	}
	switch {
	case len(seen) != 1:
		return ""
	case seen["no"]:
		return "allow"
	default:
		return "block"
	}
}

var severityRe = regexp.MustCompile(`<severity>\s*(\d+(?:\.\d+)?)\s*(</severity>)?`)

// classifierSeverity mirrors Claude Code's severity mode (2.1.328, on by default for some models): closed thinking is
// removed, an unclosed <thinking> gives no verdict, and exactly one <severity>N must remain. The block threshold is
// in the client configuration, which the proxy cannot see, so every score becomes an event and the admin judges it.
func classifierSeverity(text string) (string, bool) {
	t := thinkingRe.ReplaceAllString(text, "")
	if strings.Contains(t, "<thinking>") {
		return "", false
	}
	m := severityRe.FindAllStringSubmatch(t, -1)
	if len(m) != 1 {
		return "", false
	}
	return m[0][1], true
}

// replyText collects the text of a reply, JSON or server-sent events, up to classifierReplyCap.
type replyText struct {
	sse  bool
	raw  bytes.Buffer
	over bool
}

func (r *replyText) Write(p []byte) {
	if r.over || r.raw.Len()+len(p) > classifierReplyCap {
		r.over = true
		return
	}
	r.raw.Write(p)
}

func (r *replyText) text() (string, bool) {
	if r.over {
		return "", false
	}
	if !r.sse {
		var body struct {
			Content []msgContent `json:"content"`
		}
		if json.Unmarshal(r.raw.Bytes(), &body) != nil {
			return "", false
		}
		var b strings.Builder
		for _, c := range body.Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		return b.String(), true
	}
	var b strings.Builder
	for _, line := range strings.Split(r.raw.String(), "\n") {
		data, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "data:")
		if !ok {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(data)), &ev) == nil && ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" {
			b.WriteString(ev.Delta.Text)
		}
	}
	return b.String(), true
}

// classifierBody passes the reply through unchanged and decides once, at EOF or Close.
type classifierBody struct {
	io.ReadCloser
	r     replyText
	once  sync.Once
	agent string
	call  *classifierCall
}

func (b *classifierBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.r.Write(p[:n])
	}
	if err != nil {
		b.finish()
	}
	return n, err
}

func (b *classifierBody) Close() error {
	b.finish()
	return b.ReadCloser.Close()
}

func (b *classifierBody) finish() {
	b.once.Do(func() {
		text, ok := b.r.text()
		if !ok {
			classifierLogOnce("reply", "classifier reply not readable")
			return
		}
		verdict := classifierVerdict(text)
		sev, hasSev := classifierSeverity(text)
		what := clip(b.call.tool, 100) + ": " + clip(redactSecrets(b.call.input), classifierInputMax)
		// Claude Code parses only the format of its configured mode, which the proxy cannot see. So each format present
		// is read on its own and gives its event: a reply with <block>no and <severity>N is an allow in either mode.
		if verdict == "allow" {
			emitEvent(b.agent, "classifier-allow", "low", "auto mode classifier allowed "+what)
		}
		if hasSev {
			emitEvent(b.agent, "classifier-severity", "low", "auto mode classifier severity "+sev+" for "+what)
		}
		if verdict == "" && !hasSev {
			classifierLogOnce("verdict", "classifier reply without a verdict")
		}
	})
}

// Redaction as for the forge sidecar's run events and approval requests (images/forge approvals.go, runevents.go).
var (
	secretRe = regexp.MustCompile(`(?i)(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|` +
		`sk-[A-Za-z0-9_-]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----|` +
		`((?:password|passwd|secret|token|api[_-]?key|authorization)["']?\s*[:=]\s*["']?(?:bearer\s+|token\s+)?)[^\s"',}]{6,})`)
	bearerRe  = regexp.MustCompile(`(?i)(bearer\s+)[^\s"',}]{12,}`)
	keyTailRe = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*$`)
)

func redactSecrets(v string) string {
	v = secretRe.ReplaceAllStringFunc(v, func(m string) string {
		if sm := secretRe.FindStringSubmatch(m); sm[2] != "" {
			return sm[2] + "[redacted]"
		}
		return "[redacted]"
	})
	v = bearerRe.ReplaceAllString(v, "${1}[redacted]")
	return keyTailRe.ReplaceAllString(v, "[redacted]")
}
