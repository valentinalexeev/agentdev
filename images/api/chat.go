package main

// Chat sessions (#346, docs/console-ux.md §10): the console's Chat page talks to interactive Claude Code sessions
// through these routes. A session is a Job in the agents namespace (chat_kube.go) that runs chat-session-server
// (images/worker/chat-session-server.js). The API is the only client of that server: it proxies messages and
// permission answers, copies the event log into Postgres (chat_store.go) and streams it to browsers.
//
// Routes (bearer token, like every /api route):
//
//	GET    /api/sessions                          sessions of the last CHAT_RETENTION_DAYS the caller may read
//	POST   /api/sessions                          {"agent", "context"?}: start a session (write access)
//	GET    /api/sessions/{id}                     one session with its live state (read access)
//	DELETE /api/sessions/{id}                     end it now (write access)
//	GET    /api/sessions/{id}/events?after=N      SSE: the transcript after event id N, then live (read access)
//	POST   /api/sessions/{id}/messages            {"text"}: a message as the caller (write access)
//	POST   /api/sessions/{id}/permissions/{pid}   {"behavior": "allow"|"deny"}: answer once (write access)
//	POST   /api/sessions/{id}/drafts/{did}/create {"title"?, "body"?}: create the issue of a draft (write access)
//
// Rules:
//   - Read access to the agent's repository on the forge (live check) lets a person list and read sessions and
//     transcripts; write access lets them start, send, answer, end and create issues. Admins (ADMIN_GROUP) may read
//     and end every session; they need write access to send. A person without read access sees nothing (404).
//   - The session never creates issues: drafts are events, and only POST .../create (a person's click) files one.
//   - Every change is an audit entry (api_audit), with the person and the session.
//   - The browser never reaches the session pod. The pod accepts only the API (NetworkPolicy) with a per-session
//     token: HMAC-SHA256(CHAT_TOKEN_KEY, session id). The pod gets only its SHA-256.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	chatPoll       = time.Second
	chatHeartbeat  = 15 * time.Second
	chatStreamAge  = 30 * time.Minute
	chatPresenceTO = 45 * time.Second // a viewer counts as gone this long after its stream closed
	chatIDRe       = regexp.MustCompile(`^chat-[a-z0-9-]{1,40}-[0-9a-f]{8}$`)
	chatSubIDRe    = regexp.MustCompile(`^[pd][0-9]{1,9}$`)
)

// chatCluster runs session Jobs (chat_kube.go; a fake in tests).
type chatCluster interface {
	StartSession(ctx context.Context, s chatSession, tokenSHA256 string) error
	// SessionAddr gives host:port of the session server, "" while the pod is not ready, errChatGone when the Job is gone.
	SessionAddr(ctx context.Context, id string) (string, error)
	StopSession(ctx context.Context, id string) error
}

var errChatGone = errors.New("session job is gone")

type chatConfig struct {
	TokenKey   []byte
	MaxTotal   int // running sessions on the instance; 0 = no cap
	MaxPerUser int
	Retention  time.Duration
}

type chatAPI struct {
	w       *writer
	agents  agentLister
	store   chatStore
	cluster chatCluster
	issues  issueCreator
	cfg     chatConfig
	hc      *http.Client

	mu      sync.Mutex
	pumps   map[string]context.CancelFunc
	viewers map[string]map[string]time.Time // session -> person -> last seen (zero while a stream is open)
	open    map[string]map[string]int       // session -> person -> open streams
	states  map[string]json.RawMessage      // last state from the session server
	startMu sync.Mutex
	ctx     context.Context
	newID   func(agent string) string
	nowFn   func() time.Time
}

func newChatAPI(ctx context.Context, w *writer, agents agentLister, store chatStore, cluster chatCluster, issues issueCreator, cfg chatConfig) *chatAPI {
	c := &chatAPI{w: w, agents: agents, store: store, cluster: cluster, issues: issues, cfg: cfg,
		hc:    &http.Client{Timeout: 10 * time.Second},
		pumps: map[string]context.CancelFunc{}, viewers: map[string]map[string]time.Time{}, open: map[string]map[string]int{},
		states: map[string]json.RawMessage{}, ctx: ctx, nowFn: func() time.Time { return time.Now().UTC() }}
	c.newID = func(agent string) string {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		a := agent
		if len(a) > 40 {
			a = strings.TrimRight(a[:40], "-")
		}
		return "chat-" + a + "-" + hex.EncodeToString(b)
	}
	return c
}

func (c *chatAPI) routes(mux *http.ServeMux, verify verifier) {
	h := func(action string, f http.HandlerFunc) http.Handler {
		if action == "" {
			return auth(verify, f)
		}
		return auditAuth(action, auth(verify, f))
	}
	mux.Handle("GET /api/sessions", h("", c.list))
	mux.Handle("POST /api/sessions", h("chat-start", c.start))
	mux.Handle("GET /api/sessions/{id}", h("", c.get))
	mux.Handle("DELETE /api/sessions/{id}", h("chat-end", c.end))
	mux.Handle("GET /api/sessions/{id}/events", h("", c.events))
	mux.Handle("POST /api/sessions/{id}/messages", h("chat-message", c.message))
	mux.Handle("POST /api/sessions/{id}/permissions/{pid}", h("chat-permission", c.permission))
	mux.Handle("POST /api/sessions/{id}/drafts/{did}/create", h("chat-create-issue", c.createIssue))
}

// resume restarts the pumps of sessions that were running when the API stopped, and prunes old transcripts daily.
func (c *chatAPI) resume() {
	list, err := c.store.ListSessions(c.ctx, c.nowFn().Add(-24*time.Hour))
	if err != nil {
		log.Printf("chat: list sessions to resume: %v", err)
	}
	for _, s := range list {
		if s.EndedAt == nil {
			c.startPump(s.ID)
		}
	}
	go func() {
		for {
			if n, err := c.store.PruneChat(c.ctx); err != nil {
				log.Printf("prune chat transcripts: %v", err)
			} else if n > 0 {
				log.Printf("pruned %d chat sessions past the retention", n)
			}
			if !sleepCtx(c.ctx, 24*time.Hour) {
				return
			}
		}
	}()
}

func (c *chatAPI) token(id string) string {
	m := hmac.New(sha256.New, c.cfg.TokenKey)
	m.Write([]byte("agentdev-chat\x00" + id))
	return hex.EncodeToString(m.Sum(nil))
}

func tokenSHA(t string) string { s := sha256.Sum256([]byte(t)); return hex.EncodeToString(s[:]) }

// ---- permissions ----

func (c *chatAPI) ident(r *http.Request) *identity {
	id, _ := r.Context().Value(ctxIdentity{}).(*identity)
	return id
}

func (c *chatAPI) author(id *identity, forge string) string {
	if u := forgeUserOf(id, forge); u != "" {
		return u
	}
	return "user"
}

// access gives the caller's level on the session's repository; admins read everything. A forge error is permNone.
func (c *chatAPI) access(ctx context.Context, id *identity, s *chatSession) perm {
	p, err := c.w.repoPerm(ctx, id, s.Forge, s.Repo)
	if err != nil {
		log.Printf("chat: forge permission check on %s: %v", s.Repo, err)
		p = permNone
	}
	if p < permRead && c.w.isAdmin(id) {
		return permRead
	}
	return p
}

// session loads the session and checks the level; 404 for no read access (a non-member sees nothing).
func (c *chatAPI) session(r *http.Request, need perm) (*chatSession, perm, error) {
	sid := r.PathValue("id")
	if !chatIDRe.MatchString(sid) {
		return nil, permNone, &httpErr{http.StatusNotFound, "invalid", "no such session"}
	}
	s, err := c.store.GetSession(r.Context(), sid)
	if errors.Is(err, errChatNotFound) {
		return nil, permNone, &httpErr{http.StatusNotFound, "invalid", "no such session"}
	}
	if err != nil {
		log.Printf("chat: get session %s: %v", sid, err)
		return nil, permNone, &httpErr{http.StatusBadGateway, "error", "cannot read the session"}
	}
	p := c.access(r.Context(), c.ident(r), s)
	if p < permRead {
		return nil, p, &httpErr{http.StatusNotFound, "denied", "no such session"}
	}
	if p < need {
		return s, p, denied(fmt.Sprintf("you need %s access to %s (you have %s)", need, s.Repo, p))
	}
	return s, p, nil
}

func (c *chatAPI) auditChat(r *http.Request, action string, s *chatSession, result, reason string, req any) {
	id := c.ident(r)
	e := auditEntry{Action: action, Result: result, Reason: reason, Request: req}
	if s != nil {
		e.Agent = s.Agent
		rq := map[string]any{"session": s.ID, "repo": s.Repo}
		if req != nil {
			rq["request"] = req
		}
		e.Request = rq
	}
	if id != nil {
		e.Who = c.w.who(id)
	}
	audit(e)
}

func (c *chatAPI) fail(rw http.ResponseWriter, r *http.Request, action string, s *chatSession, err error, req any) {
	var he *httpErr
	if !errors.As(err, &he) {
		log.Printf("chat %s: %v", action, err)
		he = &httpErr{http.StatusBadGateway, "error", "the session did not answer; try again"}
	}
	if action != "" {
		c.auditChat(r, action, s, he.result, he.msg, req)
	}
	writeJSON(rw, he.code, map[string]string{"error": he.msg})
}

// ---- list, get ----

type sessionView struct {
	chatSession
	Access  string          `json:"access"` // read | write | admin: what the caller may do
	Viewers []string        `json:"viewers"`
	State   json.RawMessage `json:"state,omitempty"` // live state from the session server (tokens, idleEndsAt, ...)
}

func (c *chatAPI) view(s chatSession, p perm) sessionView {
	v := sessionView{chatSession: s, Access: p.String(), Viewers: c.viewerNames(s.ID)}
	c.mu.Lock()
	v.State = c.states[s.ID]
	c.mu.Unlock()
	return v
}

func (c *chatAPI) list(rw http.ResponseWriter, r *http.Request) {
	list, err := c.store.ListSessions(r.Context(), c.nowFn().Add(-c.cfg.Retention))
	if err != nil {
		log.Printf("chat: list sessions: %v", err)
		writeJSON(rw, http.StatusBadGateway, map[string]string{"error": "cannot read sessions"})
		return
	}
	id := c.ident(r)
	cache := map[string]perm{}
	out := []sessionView{}
	for _, s := range list {
		k := s.Forge + "\x00" + s.Repo
		p, ok := cache[k]
		if !ok {
			p = c.access(r.Context(), id, &s)
			cache[k] = p
		}
		if p >= permRead {
			out = append(out, c.view(s, p))
		}
	}
	writeJSON(rw, http.StatusOK, map[string]any{"sessions": out})
}

func (c *chatAPI) get(rw http.ResponseWriter, r *http.Request) {
	s, p, err := c.session(r, permRead)
	if err != nil {
		c.fail(rw, r, "", s, err, nil)
		return
	}
	writeJSON(rw, http.StatusOK, c.view(*s, p))
}

// ---- start, end ----

const maxContext = 8 << 10

func (c *chatAPI) start(rw http.ResponseWriter, r *http.Request) {
	const action = "chat-start"
	m, err := decodeObject(r)
	if err != nil {
		c.fail(rw, r, action, nil, err, nil)
		return
	}
	var req struct {
		Agent   string `json:"agent"`
		Context string `json:"context"`
	}
	for k := range m {
		if k != "agent" && k != "context" {
			c.fail(rw, r, action, nil, invalid(fmt.Sprintf("unknown field %q (allowed: agent, context)", k)), nil)
			return
		}
	}
	_ = json.Unmarshal(m["agent"], &req.Agent)
	if m["context"] != nil {
		if json.Unmarshal(m["context"], &req.Context) != nil || len(req.Context) > maxContext {
			c.fail(rw, r, action, nil, invalid(fmt.Sprintf("context must be text up to %d bytes", maxContext)), nil)
			return
		}
	}
	if !agentNameRe.MatchString(req.Agent) || len(req.Agent) > 40 {
		c.fail(rw, r, action, nil, invalid("agent must be the name of an agent"), map[string]string{"agent": req.Agent})
		return
	}
	list, err := c.agents.List(r.Context())
	if err != nil {
		c.fail(rw, r, action, nil, &httpErr{http.StatusBadGateway, "error", "cannot read agents"}, nil)
		return
	}
	var ag *agent
	for i := range list {
		if list[i].Name == req.Agent && len(list[i].Repos) > 0 {
			ag = &list[i]
		}
	}
	if ag == nil {
		c.fail(rw, r, action, nil, &httpErr{http.StatusNotFound, "invalid", "no such agent"}, map[string]string{"agent": req.Agent})
		return
	}
	id := c.ident(r)
	s := chatSession{Agent: ag.Name, Repo: ag.Repos[0], Forge: ag.Forge, StartedBy: c.author(id, ag.Forge), StartedAt: c.nowFn()}
	if s.Forge == "" {
		s.Forge = "gitea"
	}
	// write access on the repository, checked live (admins too: a session acts in the project)
	if _, err := c.w.authorize(r.Context(), id, s.Forge, s.Repo, permWrite, false); err != nil {
		c.fail(rw, r, action, &s, err, nil)
		return
	}
	if ag.Paused {
		c.fail(rw, r, action, &s, &httpErr{http.StatusConflict, "denied", "the agent is paused (usage limit or by a person); its budget cannot start a session"}, nil)
		return
	}
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if err := c.checkCaps(r.Context(), s.StartedBy); err != nil {
		c.fail(rw, r, action, &s, err, nil)
		return
	}
	s.ID = c.newID(s.Agent)
	if err := c.store.CreateSession(r.Context(), s); err != nil {
		c.fail(rw, r, action, &s, err, nil)
		return
	}
	if err := c.cluster.StartSession(r.Context(), s, tokenSHA(c.token(s.ID))); err != nil {
		_ = c.store.EndSession(context.Background(), s.ID, "could not start", c.nowFn())
		c.fail(rw, r, action, &s, err, nil)
		return
	}
	c.appendAPI(s.ID, "start", s.StartedBy, map[string]any{"agent": s.Agent, "repo": s.Repo})
	if req.Context != "" {
		// the item from Discuss: fenced as untrusted text, sent once the session server is up
		go c.sendWhenReady(s, "<untrusted-context>\n"+strings.ReplaceAll(req.Context, "</untrusted-context>", "")+"\n</untrusted-context>")
	}
	c.startPump(s.ID)
	c.auditChat(r, action, &s, "ok", "", nil)
	writeJSON(rw, http.StatusCreated, c.view(s, permWrite))
}

func (c *chatAPI) checkCaps(ctx context.Context, who string) error {
	if c.cfg.MaxTotal <= 0 && c.cfg.MaxPerUser <= 0 {
		return nil
	}
	list, err := c.store.ListSessions(ctx, c.nowFn().Add(-9*time.Hour))
	if err != nil {
		return &httpErr{http.StatusServiceUnavailable, "error", "cannot count the sessions; try again later"}
	}
	total, mine := 0, 0
	for _, s := range list {
		if s.EndedAt == nil {
			total++
			if s.StartedBy == who {
				mine++
			}
		}
	}
	if c.cfg.MaxTotal > 0 && total >= c.cfg.MaxTotal {
		return denied(fmt.Sprintf("the instance limit of %d running sessions is reached; end a session first", c.cfg.MaxTotal))
	}
	if c.cfg.MaxPerUser > 0 && mine >= c.cfg.MaxPerUser {
		return denied(fmt.Sprintf("your limit of %d running sessions is reached; end one of your sessions first", c.cfg.MaxPerUser))
	}
	return nil
}

func (c *chatAPI) end(rw http.ResponseWriter, r *http.Request) {
	const action = "chat-end"
	s, p, err := c.session(r, permWrite)
	if err != nil && !(s != nil && c.w.isAdmin(c.ident(r))) { // admins may end any session (emergency stop)
		c.fail(rw, r, action, s, err, nil)
		return
	}
	who := c.author(c.ident(r), s.Forge)
	if s.EndedAt == nil {
		// ask the server to end (writes the end event), then remove the Job in any case
		_, _ = c.callSession(r.Context(), s.ID, http.MethodPost, "/end", map[string]string{"author": who})
		c.pumpOnce(s.ID)
		if err := c.cluster.StopSession(r.Context(), s.ID); err != nil && !errors.Is(err, errChatGone) {
			log.Printf("chat: stop %s: %v", s.ID, err)
		}
		c.finish(s.ID, "ended by "+who)
	}
	c.auditChat(r, action, s, "ok", "", nil)
	s, _ = c.store.GetSession(r.Context(), s.ID)
	writeJSON(rw, http.StatusOK, c.view(*s, p))
}

// ---- messages, permissions ----

const maxMessage = 64 << 10

func (c *chatAPI) live(r *http.Request, action string, rw http.ResponseWriter) (*chatSession, bool) {
	s, _, err := c.session(r, permWrite)
	if err == nil && s.EndedAt != nil {
		err = &httpErr{http.StatusConflict, "invalid", "the session has ended; the transcript is read-only"}
	}
	if err != nil {
		c.fail(rw, r, action, s, err, nil)
		return nil, false
	}
	return s, true
}

func (c *chatAPI) message(rw http.ResponseWriter, r *http.Request) {
	const action = "chat-message"
	s, ok := c.live(r, action, rw)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxMessage+1024)).Decode(&body); err != nil || strings.TrimSpace(body.Text) == "" || len(body.Text) > maxMessage {
		c.fail(rw, r, action, s, invalid(fmt.Sprintf("text must be 1 to %d bytes", maxMessage)), nil)
		return
	}
	who := c.author(c.ident(r), s.Forge)
	if _, err := c.callSession(r.Context(), s.ID, http.MethodPost, "/messages", map[string]string{"author": who, "text": body.Text}); err != nil {
		c.fail(rw, r, action, s, err, nil)
		return
	}
	c.pumpOnce(s.ID)
	c.auditChat(r, action, s, "ok", "", map[string]int{"bytes": len(body.Text)}) // the text is in the transcript, not the audit
	writeJSON(rw, http.StatusAccepted, map[string]string{"status": "sent"})
}

func (c *chatAPI) permission(rw http.ResponseWriter, r *http.Request) {
	const action = "chat-permission"
	s, ok := c.live(r, action, rw)
	if !ok {
		return
	}
	pid := r.PathValue("pid")
	var body struct {
		Behavior string `json:"behavior"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	if !chatSubIDRe.MatchString(pid) || pid[0] != 'p' || (body.Behavior != "allow" && body.Behavior != "deny") {
		c.fail(rw, r, action, s, invalid("behavior must be allow or deny"), map[string]string{"permission": pid})
		return
	}
	req := map[string]string{"permission": pid, "behavior": body.Behavior}
	who := c.author(c.ident(r), s.Forge)
	if _, err := c.callSession(r.Context(), s.ID, http.MethodPost, "/permissions/"+pid, map[string]string{"author": who, "behavior": body.Behavior}); err != nil {
		c.fail(rw, r, action, s, err, req)
		return
	}
	c.pumpOnce(s.ID)
	c.auditChat(r, action, s, "ok", "", req)
	writeJSON(rw, http.StatusOK, map[string]string{"status": body.Behavior})
}

// ---- issue drafts ----

// issueCreator files an issue on the forge for a person (chat_issue.go).
type issueCreator interface {
	CreateIssue(ctx context.Context, forge, repo, asUser, title, body string) (number int, url string, err error)
}

type draftData struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	What       string `json:"what"`
	Acceptance string `json:"acceptance"`
	Decisions  string `json:"decisions"`
}

func (d draftData) body() string {
	b := "## What\n\n" + d.What + "\n\n## Acceptance criterion\n\n" + d.Acceptance + "\n"
	if strings.TrimSpace(d.Decisions) != "" {
		b += "\n## Open decisions\n\n" + d.Decisions + "\n"
	}
	return b
}

func (c *chatAPI) createIssue(rw http.ResponseWriter, r *http.Request) {
	const action = "chat-create-issue"
	s, _, err := c.session(r, permWrite) // also after the end: a draft in the transcript can still be filed
	if err != nil {
		c.fail(rw, r, action, s, err, nil)
		return
	}
	did := r.PathValue("did")
	if !chatSubIDRe.MatchString(did) || did[0] != 'd' {
		c.fail(rw, r, action, s, invalid("no such draft"), nil)
		return
	}
	var edit struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&edit); err != nil && !errors.Is(err, io.EOF) {
		c.fail(rw, r, action, s, invalid("body must be a JSON object"), nil)
		return
	}
	draft, created, err := c.findDraft(r.Context(), s.ID, did)
	if err != nil {
		c.fail(rw, r, action, s, err, map[string]string{"draft": did})
		return
	}
	if created != nil {
		writeJSON(rw, http.StatusConflict, map[string]any{"error": "this draft is already an issue", "issue": created})
		return
	}
	title, body := draft.Title, draft.body()
	if edit.Title != nil {
		title = strings.TrimSpace(*edit.Title)
	}
	if edit.Body != nil {
		body = *edit.Body
	}
	if title == "" || len(title) > 200 || strings.ContainsAny(title, "\r\n") || len(body) > 60000 {
		c.fail(rw, r, action, s, invalid("title must be one line of 1 to 200 characters, body up to 60000"), map[string]string{"draft": did})
		return
	}
	who := c.author(c.ident(r), s.Forge)
	body += fmt.Sprintf("\n---\nCreated by @%s from the agentdev console chat session `%s`.\n", who, s.ID)
	if c.issues == nil {
		c.fail(rw, r, action, s, &httpErr{http.StatusServiceUnavailable, "error", "issue creation is not configured on this instance"}, nil)
		return
	}
	n, url, err := c.issues.CreateIssue(r.Context(), s.Forge, s.Repo, who, title, body)
	if err != nil {
		log.Printf("chat: create issue in %s: %v", s.Repo, err)
		c.fail(rw, r, action, s, &httpErr{http.StatusBadGateway, "error", "the forge did not create the issue; try again"}, map[string]string{"draft": did})
		return
	}
	issue := map[string]any{"draft": did, "number": n, "url": url, "title": title}
	c.appendAPI(s.ID, "issue-created", who, issue)
	c.auditChat(r, action, s, "ok", "", issue)
	writeJSON(rw, http.StatusCreated, issue)
}

// findDraft returns the draft event with this id, and the issue-created event of it if one exists.
func (c *chatAPI) findDraft(ctx context.Context, sid, did string) (*draftData, map[string]any, error) {
	var found *draftData
	var created map[string]any
	var after int64
	for {
		evs, err := c.store.Events(ctx, sid, after, 500)
		if err != nil {
			return nil, nil, err
		}
		for _, e := range evs {
			switch e.Type {
			case "draft":
				var d draftData
				if json.Unmarshal(e.Data, &d) == nil && d.ID == did && e.Seq > 0 {
					found = &d
				}
			case "issue-created":
				var m map[string]any
				if json.Unmarshal(e.Data, &m) == nil && m["draft"] == did && e.Seq == 0 {
					created = m
				}
			}
			after = e.ID
		}
		if len(evs) < 500 {
			break
		}
	}
	if found == nil {
		return nil, nil, &httpErr{http.StatusNotFound, "invalid", "no such draft"}
	}
	return found, created, nil
}

// ---- event stream ----

func (c *chatAPI) events(rw http.ResponseWriter, r *http.Request) {
	s, p, err := c.session(r, permRead)
	if err != nil {
		c.fail(rw, r, "", s, err, nil)
		return
	}
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 || !digitsAnyRe.MatchString(v) {
			writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "after must be an event id"})
			return
		}
		after = n
	}
	sse, err := newChatSSE(rw)
	if err != nil {
		return
	}
	who := c.author(c.ident(r), s.Forge)
	live := s.EndedAt == nil
	if live {
		c.join(s.ID, who)
		defer c.leave(s.ID, who)
	}
	if sse.send("hello", map[string]any{"heartbeatSeconds": int(chatHeartbeat.Seconds()), "session": c.view(*s, p)}) != nil {
		return
	}
	ctx := r.Context()
	deadline := time.Now().Add(chatStreamAge)
	beat := time.Now()
	var lastState string
	for {
		evs, err := c.store.Events(ctx, s.ID, after, 500)
		if err != nil {
			_ = sse.send("bye", map[string]string{"reason": "cannot read the transcript"})
			return
		}
		for _, e := range evs {
			if sse.send("chat-event", e) != nil {
				return
			}
			after = e.ID
		}
		cur, err := c.store.GetSession(ctx, s.ID)
		if err == nil {
			v := c.view(*cur, p)
			if b, _ := json.Marshal(v); string(b) != lastState {
				lastState = string(b)
				if sse.send("session", v) != nil {
					return
				}
			}
			if cur.EndedAt != nil && len(evs) < 500 && !live {
				_ = sse.send("bye", map[string]string{"reason": "the session has ended"})
				return
			}
			live = cur.EndedAt == nil
		}
		if len(evs) == 500 {
			continue
		}
		if time.Now().After(deadline) {
			_ = sse.send("bye", map[string]string{"reason": "reconnect"})
			return
		}
		if time.Since(beat) >= chatHeartbeat {
			beat = time.Now()
			if sse.send("heartbeat", nil) != nil {
				return
			}
		}
		if !sleepCtx(ctx, chatPoll) {
			return
		}
	}
}

var digitsAnyRe = regexp.MustCompile(`^[0-9]{1,18}$`)

// chatSSE writes server-sent events with the envelope of GET /api/stream (#345): event, id, data
// {"type", "time", "data"}. Types here: hello, chat-event, session, heartbeat, bye.
type chatSSE struct {
	w   http.ResponseWriter
	rc  *http.ResponseController
	seq int64
}

func newChatSSE(w http.ResponseWriter) (*chatSSE, error) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	c := &chatSSE{w: w, rc: rc}
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return c, rc.Flush()
}

func (c *chatSSE) send(typ string, data any) error {
	b, err := json.Marshal(struct {
		Type string    `json:"type"`
		Time time.Time `json:"time"`
		Data any       `json:"data"`
	}{typ, time.Now().UTC(), data})
	if err != nil {
		return err
	}
	c.seq++
	_ = c.rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(c.w, "event: %s\nid: %d\ndata: %s\n\n", typ, c.seq, b); err != nil {
		return err
	}
	return c.rc.Flush()
}

// ---- presence ----

func (c *chatAPI) join(sid, who string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.open[sid] == nil {
		c.open[sid] = map[string]int{}
		c.viewers[sid] = map[string]time.Time{}
	}
	c.open[sid][who]++
	c.viewers[sid][who] = time.Time{}
}

func (c *chatAPI) leave(sid, who string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.open[sid][who]--
	if c.open[sid][who] <= 0 {
		delete(c.open[sid], who)
		c.viewers[sid][who] = c.nowFn()
	}
}

// viewerNames: people with an open stream, or one closed less than chatPresenceTO ago (a page reload).
func (c *chatAPI) viewerNames(sid string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []string{}
	for who, t := range c.viewers[sid] {
		if t.IsZero() || c.nowFn().Sub(t) < chatPresenceTO {
			out = append(out, who)
		} else {
			delete(c.viewers[sid], who)
		}
	}
	sortStrings(out)
	return out
}

// ---- the pump: session server -> store ----

func (c *chatAPI) startPump(sid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.pumps[sid]; ok {
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.pumps[sid] = cancel
	go c.pump(ctx, sid)
}

func (c *chatAPI) pump(ctx context.Context, sid string) {
	misses := 0
	for {
		gone, err := c.pumpStep(ctx, sid)
		switch {
		case gone:
			c.finish(sid, "the session pod is gone")
			return
		case err != nil:
			misses++
			if misses == 1 || misses%60 == 0 {
				log.Printf("chat: session %s: %v", sid, err)
			}
		default:
			misses = 0
		}
		s, err := c.store.GetSession(ctx, sid)
		if err == nil && s.EndedAt != nil {
			c.stopPump(sid)
			return
		}
		if !sleepCtx(ctx, chatPoll) {
			return
		}
	}
}

// pumpOnce copies new events now (after a message or an answer, so the caller sees it at once).
func (c *chatAPI) pumpOnce(sid string) {
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	_, _ = c.pumpStep(ctx, sid)
}

// pumpStep sends the presence, reads new events and stores them. gone: the Job no longer exists.
func (c *chatAPI) pumpStep(ctx context.Context, sid string) (bool, error) {
	s, err := c.store.GetSession(ctx, sid)
	if err != nil {
		return false, err
	}
	if _, err := c.callSession(ctx, sid, http.MethodPost, "/presence", map[string]any{"viewers": c.viewerNames(sid)}); err != nil {
		if errors.Is(err, errChatGone) {
			return true, nil
		}
		return false, err
	}
	b, err := c.callSession(ctx, sid, http.MethodGet, "/events?after="+strconv.FormatInt(s.LastSeq, 10), nil)
	if err != nil {
		return errors.Is(err, errChatGone), err
	}
	var resp struct {
		Events []struct {
			Seq    int64           `json:"seq"`
			Time   time.Time       `json:"time"`
			Type   string          `json:"type"`
			Author string          `json:"author"`
			Data   json.RawMessage `json:"data"`
		} `json:"events"`
		State json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return false, fmt.Errorf("bad events answer: %w", err)
	}
	evs := make([]chatEvent, 0, len(resp.Events))
	ended := ""
	var upTo int64
	for _, e := range resp.Events {
		if e.Seq > upTo {
			upTo = e.Seq
		}
		if e.Seq <= s.LastSeq || !chatEventTypes[e.Type] || len(e.Data) == 0 || len(e.Data) > 256<<10 {
			continue
		}
		evs = append(evs, chatEvent{Session: sid, Seq: e.Seq, Time: e.Time, Type: e.Type, Author: e.Author, Data: e.Data})
		if e.Type == "end" {
			var d struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(e.Data, &d)
			ended = d.Reason
			if ended == "" {
				ended = "ended"
			}
		}
	}
	if err := c.store.AppendEvents(ctx, sid, evs, upTo); err != nil {
		return false, err
	}
	c.mu.Lock()
	if len(resp.State) > 0 {
		c.states[sid] = resp.State
	}
	c.mu.Unlock()
	if ended != "" {
		c.finish(sid, ended)
	}
	return false, nil
}

// chatEventTypes: the types the session server may write. Anything else is dropped. issue-created and start are
// written by the API only (seq 0).
var chatEventTypes = map[string]bool{"start": true, "ready": true, "user": true, "assistant": true, "tool": true,
	"tool-result": true, "turn": true, "permission": true, "permission-answer": true, "draft": true, "presence": true, "end": true}

func (c *chatAPI) finish(sid, reason string) {
	if err := c.store.EndSession(context.Background(), sid, reason, c.nowFn()); err != nil {
		log.Printf("chat: end %s: %v", sid, err)
	}
	c.stopPump(sid)
}

func (c *chatAPI) stopPump(sid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cancel, ok := c.pumps[sid]; ok {
		cancel()
		delete(c.pumps, sid)
	}
	delete(c.states, sid)
}

// appendAPI writes an event the API itself is the source of (seq 0).
func (c *chatAPI) appendAPI(sid, typ, author string, data any) {
	b, _ := json.Marshal(data)
	if err := c.store.AppendEvents(context.Background(), sid, []chatEvent{{Session: sid, Time: c.nowFn(), Type: typ, Author: author, Data: b}}, 0); err != nil {
		log.Printf("chat: store %s event of %s: %v", typ, sid, err)
	}
}

func (c *chatAPI) sendWhenReady(s chatSession, text string) {
	for i := 0; i < 120; i++ {
		if _, err := c.callSession(c.ctx, s.ID, http.MethodPost, "/messages", map[string]string{"author": s.StartedBy, "text": text}); err == nil {
			return
		} else if errors.Is(err, errChatGone) {
			return
		}
		if !sleepCtx(c.ctx, 2*time.Second) {
			return
		}
	}
	log.Printf("chat: session %s did not start in time for its context", s.ID)
}

// callSession calls the session server of a session with its token.
func (c *chatAPI) callSession(ctx context.Context, sid, method, path string, body any) ([]byte, error) {
	addr, err := c.cluster.SessionAddr(ctx, sid)
	if err != nil {
		return nil, err
	}
	if addr == "" {
		return nil, &httpErr{http.StatusServiceUnavailable, "error", "the session is still starting; try again in a moment"}
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token(sid))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		code := resp.StatusCode
		if code >= 500 || code == 401 || code == 403 {
			return nil, fmt.Errorf("session server: HTTP %d %s", code, e.Error)
		}
		return nil, &httpErr{code, "invalid", "the session refused: " + e.Error}
	}
	return b, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
