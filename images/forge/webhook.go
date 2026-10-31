package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// The webhook listener (was webhook-listener.js in the agent container): the router calls the pod port with the
// agent's WAKE_TOKEN; a valid call appends one line to WAKE_FILE (on the shared /state), and agent-loop wakes when
// the line count grows. Registration with the router (REGISTER_TOKEN) runs here too, so neither token is in the
// agent container.

var eventRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func appendWake(path, event string) error {
	if !eventRe.MatchString(event) {
		event = "unknown"
	}
	// O_NOFOLLOW: the agent container can write /state, so the file could be swapped for a symlink
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o664)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	_, err = fmt.Fprintf(f, "%d %s\n", time.Now().UnixMilli(), event)
	return err
}

func wakeHandler(c *Config, logf func(string, ...any)) http.Handler {
	want := []byte("Bearer " + c.WakeToken)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if got := []byte(r.Header.Get("Authorization")); len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			logf("rejected wake (token)")
			emitEvent(c.AgentName, "", "webhook-bad-token", "medium", fmt.Sprintf("POST %s from %s", r.URL.Path, r.RemoteAddr))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var ev struct {
			Event string `json:"event"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&ev)
		if err := appendWake(c.WakeFile, ev.Event); err != nil {
			logf("wake: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		logf("wake (%s)", ev.Event)
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func (c *Config) webhooksOn() bool {
	return c.RouterURL != "" && c.WakeToken != "" && c.RegisterToken != "" && c.SelfURL != ""
}

func startWebhook(c *Config, logf func(string, ...any)) {
	if !c.webhooksOn() {
		logf("ROUTER_URL, WAKE_TOKEN, REGISTER_TOKEN or SELF_URL not set: webhooks off, polling only")
		return
	}
	srv := &http.Server{Addr: c.WebhookListen, Handler: wakeHandler(c, logf), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		logf("webhook listener on %s", c.WebhookListen)
		logf("webhook listener: %v", srv.ListenAndServe())
	}()
	go registerLoop(c, logf)
}

func registerOnce(c *Config) error {
	body, _ := json.Marshal(map[string]any{"agent": c.AgentName, "forge": c.ForgeKind, "user": c.ForgeUser,
		"repos": c.Repos, "url": c.SelfURL, "token": c.WakeToken})
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(c.RouterURL, "/")+"/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.RegisterToken)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// registerLoop refreshes well inside the router's TTL, and every 5 s while not registered.
func registerLoop(c *Config, logf func(string, ...any)) {
	registered := false
	last := time.Time{}
	for {
		if !registered || time.Since(last) >= c.RegisterEvery {
			err := registerOnce(c)
			switch {
			case err == nil && !registered:
				logf("registered with router %s", c.RouterURL)
			case err != nil && registered:
				logf("router registration: %v", err)
			}
			registered, last = err == nil, time.Now()
		}
		time.Sleep(5 * time.Second)
	}
}

// POST /idle (localhost API): ask the router to stop this agent; body {"idleSince": <ms>}. Answers with the
// router's JSON ({stopped, reason}).
func (s *server) idle(w http.ResponseWriter, r *http.Request) {
	if !s.c.webhooksOn() {
		http.Error(w, "webhooks off", http.StatusConflict)
		return
	}
	var in struct {
		IdleSince int64 `json:"idleSince"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	body, _ := json.Marshal(map[string]any{"agent": s.c.AgentName, "idleSince": in.IdleSince})
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.c.RouterURL, "/")+"/idle", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.c.WakeToken)
	// a timeout and no redirects (the request carries the wake token), like the forge proxy
	resp, err := s.client.Do(req)
	if err != nil {
		fail(w, fmt.Errorf("router unreachable"))
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 64<<10))
}
