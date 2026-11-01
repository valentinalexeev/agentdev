package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Agent keys (#66): the agent container calls the proxy with ANTHROPIC_AUTH_TOKEN=<agent>.<hex HMAC-SHA256(signing
// key, agent)>. The key names the agent and is worth nothing outside the cluster (the proxy is the only place that
// adds the real credential, and only for that agent's budget). The signing key is only in the proxy pod.

var agentRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func agentKey(signing []byte, agent string) string {
	m := hmac.New(sha256.New, signing)
	m.Write([]byte("agentdev-claude-proxy/v1\x00" + agent))
	return agent + "." + hex.EncodeToString(m.Sum(nil))
}

// verifyKey returns the agent named by a valid key, or "" for anything else.
func verifyKey(signing []byte, key string) string {
	agent, _, ok := strings.Cut(key, ".")
	if !ok || !agentRe.MatchString(agent) || len(signing) < 32 {
		return ""
	}
	if hmac.Equal([]byte(key), []byte(agentKey(signing, agent))) {
		return agent
	}
	return ""
}

// callerKey takes the key from the headers Claude Code sends: Authorization: Bearer (ANTHROPIC_AUTH_TOKEN) or
// x-api-key (ANTHROPIC_API_KEY).
func callerKey(auth, apiKey string) string {
	if k, ok := strings.CutPrefix(auth, "Bearer "); ok {
		return strings.TrimSpace(k)
	}
	return strings.TrimSpace(apiKey)
}
