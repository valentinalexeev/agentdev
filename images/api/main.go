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
	"strconv"
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
	// security events (#70)
	DatabaseURL string // platform Postgres (secret agentdev-api-db); empty: events kept in memory only (dev)
	DBMigrate   bool   // run schema.sql as the API login (no separate owner role; tests and standalone use)
	// SIEM outputs are read by outputsFromEnv (outputs.go) from the Secret agentdev-api-siem
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

		DatabaseURL: os.Getenv("DATABASE_URL"),
		DBMigrate:   os.Getenv("DATABASE_MIGRATE") == "true",
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
	store, err := openStore(ctx, cfg)
	if err != nil {
		log.Fatalf("security events: %v", err)
	}
	// SIEM outputs (#70 webhook, #71 syslog over TLS and OTLP): outputs.go
	outs, err := outputsFromEnv(os.Getenv)
	if err != nil {
		log.Fatalf("security outputs: %v", err)
	}
	s := &sink{store: store, outs: outs}
	for _, o := range outs {
		go o.Run(ctx)
	}
	go watchSecurityEvents(ctx, agents.d, cfg.Namespace, s)
	// FORGE_API_TOKEN (from the Secret agentdev-api-forge) only needs read access to the repositories. Without it every
	// write that needs the forge is refused (fail closed); the admin group can still pause agents.
	defaultFiveHour, defaultWeekly = envPct("USAGE_FIVE_HOUR", defaultFiveHour), envPct("USAGE_WEEKLY", defaultWeekly)
	gp := newGiteaPerms(cfg.ForgeURL, os.Getenv("FORGE_API_TOKEN"))
	// GitHub (#87): the App (GITHUB_APP_ID, GITHUB_APP_PRIVATE_KEY_FILE) or a read-only GITHUB_API_TOKEN. Without
	// either, every GitHub permission check fails closed (503); agentdev-admins can still pause, add and remove.
	gh, err := githubFromEnv(os.Getenv)
	if err != nil {
		log.Fatalf("github: %v", err)
	}
	perms := forgePerms{gitea: gp}
	if gh != nil {
		perms.github = gh
		log.Printf("github permission checks on %s (%s)", gh.api, map[bool]string{true: "App", false: "token"}[gh.key != nil])
	}
	wr := &writer{store: agents, perms: perms, adminGroup: cfg.AdminGrp, usage: agents}
	if os.Getenv("APPROVALS") == "true" { // #67: forge decisions, and the internal listener the agents' sidecars ask
		dc := newDecider(agents, agents, gp, perms)
		dc.github = gh
		dc.pods = agents
		for _, l := range strings.Split(os.Getenv("APPROVAL_DENY_ACCOUNTS"), ",") {
			if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
				dc.denyLogins[l] = true
				approvalDenyLogins[l] = true // the UI refuses the same accounts
			}
		}
		go dc.run(ctx, time.Duration(envInt("APPROVAL_POLL_SECONDS", 15))*time.Second)
		imux := http.NewServeMux()
		imux.Handle("GET /internal/approvals/{agent}/{id}", internalDecision(agents))
		imux.Handle("POST /internal/approvals/{agent}/{id}/void", internalVoid(agents))
		isrv := &http.Server{Addr: env("INTERNAL_ADDR", ":8081"), Handler: imux, ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
		go func() { log.Fatal(isrv.ListenAndServe()) }()
	}
	srv := &http.Server{Addr: cfg.Addr, Handler: routes(verify, agents, store, wr), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	log.Printf("agentdev-api on %s, issuer %s, namespace %s", cfg.Addr, cfg.Issuer, cfg.Namespace)
	log.Fatal(srv.ListenAndServe())
}

// envPct reads a percent 1..100; any other value keeps def.
func envPct(k string, def int) int {
	if n := envInt(k, def); validPct(n) {
		return n
	}
	return def
}

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil && n > 0 {
		return n
	}
	return def
}

// openStore connects to Postgres and runs the migrations, or falls back to memory when no database is configured.
func openStore(ctx context.Context, cfg config) (eventStore, error) {
	if cfg.DatabaseURL == "" {
		log.Printf("DATABASE_URL not set: security events and the audit trail are kept in memory only (lost on restart)")
		auditDB = &memAudit{}
		return newMemStore(), nil
	}
	pg, err := newPgStore(ctx, cfg.DatabaseURL, cfg.DBMigrate)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			if _, err := pg.Prune(ctx); err != nil {
				log.Printf("prune security_events: %v", err)
			}
			if _, err := pg.PruneAudit(ctx); err != nil {
				log.Printf("prune api_audit: %v", err)
			}
			if !sleepCtx(ctx, 24*time.Hour) {
				return
			}
		}
	}()
	auditDB = pg
	return pg, nil
}

// verifier checks a raw bearer token and returns the caller's identity.
type verifier func(ctx context.Context, raw string) (*identity, error)

type identity struct {
	Subject  string `json:"sub"`
	Username string `json:"preferred_username"`
	Email    string `json:"email"`
	// ForgeLogin is the custom claim forge_login (scope "forge", #88): the only source of the forge user name.
	ForgeLogin string   `json:"forge_login,omitempty"`
	Groups     []string `json:"groups,omitempty"` // authentik groups (profile scope); agentdev-admins is checked here
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
	// SecurityEvents in the last 7 days (#70)
	SecurityEvents int `json:"securityEvents"`
	// Claude tokens in the last 24 h and the agent's limit (0 = none), from the credential proxy (#66, budget.go)
	BudgetUsed  *int64 `json:"budgetUsed,omitempty"`
	BudgetLimit *int64 `json:"budgetLimit,omitempty"`
}

func routes(verify verifier, agents agentLister, events eventStore, wr *writer) http.Handler {
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
		// counter badge on the overview (#70): events of the last 7 days per agent
		if events != nil {
			counts, err := events.Counts(r.Context(), time.Now().Add(-7*24*time.Hour))
			if err != nil {
				log.Printf("count security events: %v", err)
			}
			for i := range list {
				list[i].SecurityEvents = counts[list[i].Name]
			}
		}
		addBudgets(r.Context(), list) // #66
		writeJSON(w, http.StatusOK, map[string]any{"agents": list})
	})))
	// Every authenticated user may read every event for now. Restricting the list to the projects the caller has
	// write access to needs the forge permission check of #67 and per-project access from #57: see #57.
	mux.Handle("GET /api/security-events", auth(verify, listSecurityEvents(events)))
	if wr != nil { // writes to Agent objects (#57 step 3), see write.go
		mux.Handle("PATCH /api/agents/{name}", auditAuth("patch", auth(verify, http.HandlerFunc(wr.patch))))
		mux.Handle("POST /api/agents", auditAuth("create", auth(verify, http.HandlerFunc(wr.create))))
		mux.Handle("GET /api/audit", auth(verify, listAudit(auditDB, wr.adminGroup))) // #91, admins only
		mux.Handle("DELETE /api/agents/{name}", auditAuth("delete", auth(verify, http.HandlerFunc(wr.delete))))
		if as, ok := wr.store.(approvalStore); ok { // approvals (#67, approvals.go): forge comments or the UI
			ar := &approvalReader{store: as, w: wr, agents: agents}
			mux.Handle("GET /api/approvals", auth(verify, http.HandlerFunc(ar.list)))
			mux.Handle("GET /api/approvals/{name}", auth(verify, http.HandlerFunc(ar.get)))
			mux.Handle("POST /api/approvals/{name}/decision", auditAuth("approval-decision", auth(verify, http.HandlerFunc(ar.decide))))
		}
	}
	if wr != nil && wr.usage != nil { // subscription usage limits (#93), see usage.go
		// every signed-in user, like the security events: same exposure as R35 (filter by write access: #57)
		mux.Handle("GET /api/usage", auth(verify, http.HandlerFunc(wr.getUsage)))
		mux.Handle("PUT /api/usage/global", auditAuth("set-limits", auth(verify, http.HandlerFunc(wr.putGlobal))))
		mux.Handle("PUT /api/usage/projects/{owner}/{repo}", auditAuth("set-limits", auth(verify, http.HandlerFunc(wr.putProject))))
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
