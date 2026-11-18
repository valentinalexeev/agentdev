package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeCred(t *testing.T, dir, secret string, kv map[string]string) {
	t.Helper()
	d := filepath.Join(dir, secret)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	for k, v := range kv {
		if err := os.WriteFile(filepath.Join(d, k), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCredentialCheck(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "claude-shared", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "t", "CREDENTIAL_FORMAT": "ok"})
	writeCred(t, dir, "claude-bad", map[string]string{"CREDENTIAL_FORMAT": "invalid"})
	writeCred(t, dir, "claude-old", map[string]string{"ANTHROPIC_API_KEY": "k"}) // 1.1 Secret, no marker: fine
	var reported []string
	if err := (credStore{dir}).checkCredentials(func(s string) { reported = append(reported, s) }); err != nil {
		t.Fatalf("shared is fine: %v", err)
	}
	if !reflect.DeepEqual(reported, []string{"claude-bad"}) {
		t.Fatalf("reported %v", reported)
	}
}

func TestCredentialCheckRefusesBadShared(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "claude-shared", map[string]string{"CREDENTIAL_FORMAT": "invalid"})
	if err := (credStore{dir}).checkCredentials(func(string) {}); err == nil {
		t.Fatal("an invalid claude-shared must stop the proxy")
	}
	// no secrets at all (fresh kind cluster without a credential): nothing to refuse here, as before
	if err := (credStore{t.TempDir()}).checkCredentials(func(string) {}); err != nil {
		t.Fatal(err)
	}
}
