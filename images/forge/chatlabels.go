package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Label writes of a console chat session (#571, docs/adr/0011-chat-label-writes.md). The API starts a chat pod with
// CHAT_SESSION=<session id> on this container (images/api/chat_kube.go chatPod); the agent pod never has it, so the
// autonomous loop keeps the #227 rule (checkOwnPRLabel) unchanged. In a chat pod the sidecar adds any labels to an
// issue or a PR, agent:<name> labels included, but only when a person asked for exactly this call: the sidecar asks
// the API, which answers with the person who allowed, in the console, a Bash command that names this labels path and
// every label (images/api/chat_labels.go). The model cannot write that answer: it comes from the API, which binds the
// call to the session's pod address. No answer, no write (fail closed). The API audits each label write with the
// person (api_audit, action chat-label); the sidecar log has the same line.

var (
	chatSessionRe = regexp.MustCompile(`^chat-[a-z0-9-]{1,40}-[0-9a-f]{8}$`)
	// a login as the API sends it (GitHub or Gitea user name)
	chatPersonRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,99}$`)
)

const chatMaxLabels = 10

// checkLabelWrite: the one rule for POST .../issues/N/labels. A chat pod uses the chat policy, every other pod #227.
func checkLabelWrite(s *server, ctx context.Context, m []string, body []byte) error {
	if s.c.ChatSession != "" {
		return checkChatLabel(s, ctx, m, body)
	}
	return checkOwnPRLabel(s, ctx, m, body)
}

// chatLabelName: a label name GitHub and Gitea accept (1 to 50 characters), without control or invisible characters,
// so the name the person saw in the console is the name that is sent.
func chatLabelName(l string) bool {
	if l == "" || !utf8.ValidString(l) || utf8.RuneCountInString(l) > 50 || strings.TrimSpace(l) != l {
		return false
	}
	for _, r := range l {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func checkChatLabel(s *server, ctx context.Context, m []string, body []byte) error {
	var rq map[string]json.RawMessage
	if err := json.Unmarshal(body, &rq); err != nil {
		return refuse("bad label body")
	}
	var labels []string
	for k, v := range rq {
		if k != "labels" || json.Unmarshal(v, &labels) != nil {
			return refuseAs("api-not-allowed", "medium", "label: only \"labels\" (a list of names) is allowed")
		}
	}
	if len(labels) == 0 || len(labels) > chatMaxLabels {
		return refuseAs("api-not-allowed", "medium", "label: 1 to %d label names", chatMaxLabels)
	}
	routes := false
	seen := map[string]bool{}
	for _, l := range labels {
		if !chatLabelName(l) || seen[l] {
			return refuseAs("api-not-allowed", "medium", "label: %q is not a label name, or it is there twice", clip(l, 60))
		}
		seen[l] = true
		routes = routes || strings.HasPrefix(strings.ToLower(l), "agent:")
	}
	if len(m) < 3 {
		return refuse("label: no issue number")
	}
	n, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil || n <= 0 {
		return refuse("label: no issue number")
	}
	// an agent:<name> label routes work to an agent: without a person this is an agent queueing work for itself
	sev := "medium"
	if routes {
		sev = "high"
	}
	person, err := s.chatLabelAsked(ctx, m[1], n, labels)
	if err != nil {
		return refuseAs("chat-label-not-asked", sev, "label: no person allowed this call in the console of %s (%v)", s.c.ChatSession, err)
	}
	s.logf("chat %s: labels %s on %s#%d, asked by %s", s.c.ChatSession, strings.Join(labels, ", "), m[1], n, person)
	return nil
}

// chatLabelAsked asks the API who allowed this label call in the session's console:
// POST {API}/internal/chat/{session}/label-asked {"repo","issue","labels"} -> 200 {"person"}. Anything else refuses.
func (s *server) chatLabelAsked(ctx context.Context, repo string, issue int64, labels []string) (string, error) {
	if s.c.APIURL == "" {
		return "", fmt.Errorf("no API_INTERNAL_URL")
	}
	b, _ := json.Marshal(map[string]any{"repo": repo, "issue": issue, "labels": labels})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := strings.TrimRight(s.c.APIURL, "/") + "/internal/chat/" + url.PathEscape(s.c.ChatSession) + "/label-asked"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("API unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Person string `json:"person"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out); err != nil || !chatPersonRe.MatchString(out.Person) {
		return "", fmt.Errorf("API: bad answer")
	}
	return out.Person, nil
}
