package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// userinfoCacheMax bounds the userinfo cache (#322): one entry per live access token.
const userinfoCacheMax = 1024

// newVerifier discovers the issuer once and verifies signature, issuer, audience and expiry of each token.
// authentik derives the issuer from the request host, so tokens carry the URL the user logged in through (issuer),
// while the API reaches authentik inside the cluster (discovery). Signing keys come from the discovery document.
//
// Claim contract (#322): `groups` and `forge_login*` come from the access token. A provider with minimal access
// tokens (Pocket ID) puts them only into userinfo: when the token has none of them, the API calls the discovery
// `userinfo_endpoint` with the token, requires the same `sub` (OIDC Core 5.3.2), and merges the claims. The result is
// cached per token hash until the token's exp. A userinfo error or a different subject refuses the token.
func newVerifier(ctx context.Context, issuer, discovery, audience string) (verifier, error) {
	if discovery == "" {
		discovery = issuer
	}
	// bounded discovery and key fetches: a hung authentik must not hang startup or requests
	hc := &http.Client{Timeout: 10 * time.Second}
	ctx = oidc.ClientContext(ctx, hc)
	if discovery != issuer {
		ctx = oidc.InsecureIssuerURLContext(ctx, issuer)
	}
	p, err := oidc.NewProvider(ctx, discovery)
	if err != nil {
		return nil, fmt.Errorf("discover %s: %w", discovery, err)
	}
	var meta struct {
		UserInfo string `json:"userinfo_endpoint"`
	}
	_ = p.Claims(&meta)
	cache := &userinfoCache{m: map[[32]byte]cachedIdentity{}}
	// Accepts tokens of the agentdev client (access and ID tokens share the audience). Fine while read-only; before
	// write endpoints, require access tokens only (#46 follow-up in the lifecycle work).
	v := p.Verifier(&oidc.Config{ClientID: audience})
	return func(rctx context.Context, raw string) (*identity, error) {
		tok, err := v.Verify(rctx, raw)
		if err != nil {
			return nil, err
		}
		var id identity
		if err := tok.Claims(&id); err != nil {
			return nil, err
		}
		if id.hasRoleClaims() || meta.UserInfo == "" {
			return &id, nil // authentik: claims in the token, no userinfo call
		}
		key := sha256.Sum256([]byte(raw))
		if c, ok := cache.get(key); ok {
			return &c, nil
		}
		ui, err := p.UserInfo(oidc.ClientContext(rctx, hc), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: raw, TokenType: "Bearer"}))
		if err != nil {
			return nil, fmt.Errorf("userinfo: %w", err)
		}
		if ui.Subject == "" || ui.Subject != id.Subject {
			return nil, errors.New("userinfo: subject differs from the token")
		}
		var extra identity
		if err := ui.Claims(&extra); err != nil {
			return nil, fmt.Errorf("userinfo claims: %w", err)
		}
		id.merge(extra)
		cache.put(key, id, tok.Expiry)
		return &id, nil
	}, nil
}

// hasRoleClaims: the token carries groups or a forge login, so it is authoritative (no userinfo call).
func (id *identity) hasRoleClaims() bool {
	return len(id.Groups) > 0 || id.ForgeLogin != "" || id.ForgeLoginGitHub != "" || id.ForgeLoginGitea != ""
}

// merge fills the empty fields from userinfo; the subject is never taken from userinfo.
func (id *identity) merge(u identity) {
	fill := func(dst *string, src string) {
		if *dst == "" {
			*dst = src
		}
	}
	fill(&id.Username, u.Username)
	fill(&id.Email, u.Email)
	fill(&id.ForgeLogin, u.ForgeLogin)
	fill(&id.ForgeLoginGitHub, u.ForgeLoginGitHub)
	fill(&id.ForgeLoginGitea, u.ForgeLoginGitea)
	if len(id.Groups) == 0 {
		id.Groups = u.Groups
	}
}

type cachedIdentity struct {
	identity
	exp time.Time
}

type userinfoCache struct {
	mu sync.Mutex
	m  map[[32]byte]cachedIdentity
}

func (c *userinfoCache) get(k [32]byte) (identity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || !time.Now().Before(e.exp) {
		delete(c.m, k)
		return identity{}, false
	}
	return e.identity, true
}

func (c *userinfoCache) put(k [32]byte, id identity, exp time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= userinfoCacheMax {
		now := time.Now()
		for k2, e := range c.m {
			if !now.Before(e.exp) {
				delete(c.m, k2)
			}
		}
		if len(c.m) >= userinfoCacheMax {
			clear(c.m) // still full: start again rather than grow
		}
	}
	id.Groups = append([]string(nil), id.Groups...)
	c.m[k] = cachedIdentity{identity: id, exp: exp}
}
