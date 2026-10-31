// agentdev API (#46, #12): the management API behind authentik OIDC.
// Read: overview of the agents (state from Kubernetes). Write (#57 step 3): pause, resume, edit, create, delete Agents. Every /api call needs a valid OIDC bearer
// token from the configured issuer and audience; /healthz is open.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type config struct {
	Issuer    string // OIDC issuer URL as it appears in tokens (the URL users log in through)
	Discovery string // where the API fetches the OIDC configuration (in-cluster authentik); defaults to Issuer
	Audience  string // OIDC client id of the API
	Namespace string // agents namespace
	Addr      string
	ForgeURL  string // forge API for the live permission check of writes (#57)
	AdminGrp  string // OIDC group that may create, delete and always pause agents
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	cfg := config{
		Issuer:    os.Getenv("OIDC_ISSUER"),
		Discovery: os.Getenv("OIDC_DISCOVERY_URL"),
		Audience:  os.Getenv("OIDC_AUDIENCE"),
		Namespace: env("AGENTS_NS", "coding-agents"),
		Addr:      env("ADDR", ":8080"),
		ForgeURL:  os.Getenv("FORGE_URL"),
		AdminGrp:  env("ADMIN_GROUP", "agentdev-admins"),
	}
	if cfg.Issuer == "" || cfg.Audience == "" {
		log.Fatal("OIDC_ISSUER and OIDC_AUDIENCE are required")
	}
	ctx := context.Background()
	verify, err := newVerifier(ctx, cfg.Issuer, cfg.Discovery, cfg.Audience)
	if err != nil {
		log.Fatalf("oidc: %v", err)
	}
	agents, err := newKubeAgents(cfg.Namespace)
	if err != nil {
		log.Fatalf("kubernetes: %v", err)
	}
	// FORGE_API_TOKEN (from the Secret agentdev-api-forge) only needs read access to the repositories. Without it every
	// write that needs the forge is refused (fail closed); the admin group can still pause agents.
	wr := &writer{store: agents, perms: newGiteaPerms(cfg.ForgeURL, os.Getenv("FORGE_API_TOKEN")), adminGroup: cfg.AdminGrp}
	srv := &http.Server{Addr: cfg.Addr, Handler: routes(verify, agents, wr), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	log.Printf("agentdev-api on %s, issuer %s, namespace %s", cfg.Addr, cfg.Issuer, cfg.Namespace)
	log.Fatal(srv.ListenAndServe())
}

// verifier checks a raw bearer token and returns the caller's identity.
type verifier func(ctx context.Context, raw string) (*identity, error)

type identity struct {
	Subject  string   `json:"sub"`
	Username string   `json:"preferred_username"`
	Email    string   `json:"email"`
	Groups   []string `json:"groups,omitempty"` // authentik groups (profile scope); agentdev-admins is checked here
}

// agentLister returns the agents' current state.
type agentLister interface {
	List(ctx context.Context) ([]agent, error)
}

type agent struct {
	Name          string   `json:"name"`
	Repos         []string `json:"repos"`
	Forge         string   `json:"forge,omitempty"`
	Phase         string   `json:"phase"` // running | starting | stopped | paused | blocked | invalid
	Replicas      int32    `json:"replicas"`
	Ready         int32    `json:"ready"`
	Managed       bool     `json:"managed"` // an Agent object exists (#57); false: a StatefulSet from before the operator
	MaxConcurrent int32    `json:"maxConcurrent,omitempty"`
	ScaleToZero   bool     `json:"scaleToZero"`
	Paused        bool     `json:"paused"`
}

func routes(verify verifier, agents agentLister, wr *writer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /api/me", auth(verify, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, r.Context().Value(ctxIdentity{}))
	})))
	mux.Handle("GET /api/agents", auth(verify, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		list, err := agents.List(r.Context())
		if err != nil {
			log.Printf("list agents: %v", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "cannot read agents"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"agents": list})
	})))
	if wr != nil { // writes to Agent objects (#57 step 3), see write.go
		mux.Handle("PATCH /api/agents/{name}", auth(verify, http.HandlerFunc(wr.patch)))
		mux.Handle("POST /api/agents", auth(verify, http.HandlerFunc(wr.create)))
		mux.Handle("DELETE /api/agents/{name}", auth(verify, http.HandlerFunc(wr.delete)))
	}
	return mux
}

type ctxIdentity struct{}

// auth accepts only a bearer token in the Authorization header. The API never reads cookies, so a cross-site form or
// request from another origin cannot act for a logged-in user (CSRF): the browser does not add this header by itself.

func auth(verify verifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		var raw string
		if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") { // scheme is case-insensitive (RFC 7235)
			raw = strings.TrimSpace(h[7:])
		}
		if raw == "" {
			w.Header().Set("WWW-Authenticate", `Bearer`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
			return
		}
		id, err := verify(r.Context(), raw)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxIdentity{}, id)))
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
