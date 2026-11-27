package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Credential check at the consumer (#381, was the playbook's check of #212). The External Secrets Operator writes the
// recognised keys of a Claude secret and CREDENTIAL_FORMAT=ok|invalid (infra/ansible/group_vars/all.yml eso.claude_lines).
// A secret with no recognised KEY=VALUE line would otherwise reach the proxy as an empty Secret, and the first agent
// call would fail with "no Claude credential". The proxy now refuses to start when claude-shared is invalid, and reports
// every invalid claude-<agent> as a security event (the agent falls back to claude-shared, as before). Only names and
// the state go to the log, never a value.

// credentialProblems returns the Claude Secrets in dir that are present but hold no usable credential.
func (c credStore) credentialProblems() []string {
	ents, err := os.ReadDir(c.dir)
	if err != nil {
		return nil
	}
	var bad []string
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "..") {
			continue
		}
		d := filepath.Join(c.dir, e.Name())
		format := readKey(d, "CREDENTIAL_FORMAT")
		has := readKey(d, "CLAUDE_CODE_OAUTH_TOKEN") != "" || readKey(d, "ANTHROPIC_API_KEY") != ""
		if format == "invalid" || (format == "ok" && !has) {
			bad = append(bad, e.Name())
		}
	}
	sort.Strings(bad)
	return bad
}

// checkCredentials logs and reports invalid Claude secrets; an invalid claude-shared is an error (no start).
func (c credStore) checkCredentials(report func(secret string)) error {
	for _, s := range c.credentialProblems() {
		report(s)
		if s == "claude-shared" {
			return fmt.Errorf("claude secret claude-shared has no recognised credential: put one line CLAUDE_CODE_OAUTH_TOKEN=<token> or ANTHROPIC_API_KEY=<key> in the Secret Manager entry claude-shared")
		}
	}
	return nil
}
