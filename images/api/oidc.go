package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// newVerifier discovers the issuer once and verifies signature, issuer, audience and expiry of each token.
// authentik derives the issuer from the request host, so tokens carry the URL the user logged in through (issuer),
// while the API reaches authentik inside the cluster (discovery). Signing keys come from the discovery document.
func newVerifier(ctx context.Context, issuer, discovery, audience string) (verifier, error) {
	if discovery == "" {
		discovery = issuer
	}
	// bounded discovery and key fetches: a hung authentik must not hang startup or requests
	ctx = oidc.ClientContext(ctx, &http.Client{Timeout: 10 * time.Second})
	if discovery != issuer {
		ctx = oidc.InsecureIssuerURLContext(ctx, issuer)
	}
	p, err := oidc.NewProvider(ctx, discovery)
	if err != nil {
		return nil, fmt.Errorf("discover %s: %w", discovery, err)
	}
	// Accepts tokens of the agentdev client (access and ID tokens share the audience). Fine while read-only; before
	// write endpoints, require access tokens only (#46 follow-up in the lifecycle work).
	v := p.Verifier(&oidc.Config{ClientID: audience})
	return func(ctx context.Context, raw string) (*identity, error) {
		tok, err := v.Verify(ctx, raw)
		if err != nil {
			return nil, err
		}
		var id identity
		if err := tok.Claims(&id); err != nil {
			return nil, err
		}
		return &id, nil
	}, nil
}
