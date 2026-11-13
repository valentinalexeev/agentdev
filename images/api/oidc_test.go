package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// oidcFake is a fake OIDC provider (#322): discovery, JWKS, userinfo. It signs access tokens with its own key.
type oidcFake struct {
	*httptest.Server
	key      *rsa.PrivateKey
	userinfo atomic.Value // map[string]any, or nil for HTTP 500
	calls    atomic.Int32
}

func newOIDCFake(t *testing.T) *oidcFake {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &oidcFake{key: k}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": f.URL, "jwks_uri": f.URL + "/jwks",
				"userinfo_endpoint": f.URL + "/userinfo", "authorization_endpoint": f.URL + "/auth", "token_endpoint": f.URL + "/token",
				"id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &k.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
		case "/userinfo":
			f.calls.Add(1)
			u, _ := f.userinfo.Load().(map[string]any)
			if u == nil || r.Header.Get("Authorization") == "" {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(u)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *oidcFake) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	c := map[string]any{"iss": f.URL, "aud": "agentdev", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()}
	for k, v := range claims {
		c[k] = v
	}
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(c)
	o, err := s.Sign(b)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := o.CompactSerialize()
	return raw
}

func TestVerifierUserinfoFallback(t *testing.T) {
	f := newOIDCFake(t)
	v, err := newVerifier(context.Background(), f.URL, "", "agentdev")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Pocket ID shape: bare access token, claims only in userinfo
	f.userinfo.Store(map[string]any{"sub": "u1", "groups": []string{"agentdev-admins"}, "forge_login_github": "octo", "email": "a@x"})
	bare := f.token(t, map[string]any{"sub": "u1", "scope": "openid profile email groups"})
	id, err := v(ctx, bare)
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "u1" || len(id.Groups) != 1 || id.Groups[0] != "agentdev-admins" || id.ForgeLoginGitHub != "octo" || id.Email != "a@x" {
		t.Fatalf("claims not merged: %+v", id)
	}
	// cached per token: no second call
	if _, err := v(ctx, bare); err != nil || f.calls.Load() != 1 {
		t.Fatalf("cache: %v, %d userinfo calls", err, f.calls.Load())
	}

	// subject mismatch: refused
	f.userinfo.Store(map[string]any{"sub": "other", "groups": []string{"agentdev-admins"}})
	if id, err := v(ctx, f.token(t, map[string]any{"sub": "u2"})); err == nil {
		t.Fatalf("subject mismatch accepted: %+v", id)
	}
	// userinfo without sub: refused
	f.userinfo.Store(map[string]any{"groups": []string{"agentdev-admins"}})
	if _, err := v(ctx, f.token(t, map[string]any{"sub": "u2"})); err == nil {
		t.Fatal("userinfo without sub accepted")
	}
	// userinfo error: fail closed, and not cached as a success
	f.userinfo.Store(map[string]any(nil))
	tok3 := f.token(t, map[string]any{"sub": "u3"})
	if _, err := v(ctx, tok3); err == nil {
		t.Fatal("userinfo error accepted")
	}

	// authentik shape: claims in the access token, no userinfo call
	n := f.calls.Load()
	id, err = v(ctx, f.token(t, map[string]any{"sub": "a1", "groups": []string{"agentdev-admins"}, "forge_login": "ak"}))
	if err != nil || id.ForgeLogin != "ak" || len(id.Groups) != 1 {
		t.Fatalf("authentik token: %+v %v", id, err)
	}
	if f.calls.Load() != n {
		t.Fatal("userinfo called for a token that has the claims")
	}
	// a bad signature is still refused before any userinfo call
	if _, err := v(ctx, bare[:len(bare)-4]+"AAAA"); err == nil {
		t.Fatal("bad signature accepted")
	}
}

func TestUserinfoCacheBounded(t *testing.T) {
	c := &userinfoCache{m: map[[32]byte]cachedIdentity{}}
	exp := time.Now().Add(time.Hour)
	for i := 0; i < userinfoCacheMax+10; i++ {
		var k [32]byte
		k[0], k[1] = byte(i), byte(i>>8)
		c.put(k, identity{Subject: "s"}, exp)
	}
	if len(c.m) > userinfoCacheMax {
		t.Fatalf("cache grew to %d", len(c.m))
	}
	var k [32]byte
	k[0] = 0xff
	c.put(k, identity{Subject: "old"}, time.Now().Add(-time.Second))
	if _, ok := c.get(k); ok {
		t.Fatal("expired entry returned")
	}
}
