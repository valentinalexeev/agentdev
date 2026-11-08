package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// TestSSHCALive runs against a real step-ca made by scripts/ssh-ca-init.sh --out (see docs/admin.md). Set
// SSHCA_TEST_DIR to that directory and SSHCA_TEST_URL to the CA (https://localhost:19000); skipped otherwise.
func TestSSHCALive(t *testing.T) {
	dir, url := os.Getenv("SSHCA_TEST_DIR"), os.Getenv("SSHCA_TEST_URL")
	if dir == "" || url == "" {
		t.Skip("SSHCA_TEST_DIR / SSHCA_TEST_URL not set")
	}
	prov, _ := os.ReadFile(filepath.Join(dir, "provisioner.json"))
	root, _ := os.ReadFile(filepath.Join(dir, "root_ca.crt"))
	ca, err := newSSHCA(url, "step-ca.agentdev-executors.svc", prov, root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	user, host, err := ca.Keys(ctx)
	if err != nil || len(user) != 1 || len(host) != 1 {
		t.Fatalf("keys: %v %v %v", user, host, err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	spub, _ := ssh.NewPublicKey(pub)
	c, err := ca.Sign(ctx, spub, SSHCertRequest{Type: "user", KeyID: "executor/run-1/abc", Principals: []string{"agentdev-x-check"},
		Validity: 5 * time.Minute, CriticalOptions: map[string]string{"source-address": "10.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Extensions) != 0 || c.CriticalOptions["source-address"] != "10.0.0.0/8" {
		t.Fatalf("cert: ext %v crit %v", c.Extensions, c.CriticalOptions)
	}
	userCA, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(user[0]))
	if string(c.SignatureKey.Marshal()) != string(userCA.Marshal()) {
		t.Fatal("not signed by the user CA")
	}
	hc, err := ca.Sign(ctx, spub, SSHCertRequest{Type: "host", KeyID: "host/t1", Principals: []string{"server.example"}})
	if err != nil || hc.CertType != ssh.HostCert {
		t.Fatalf("host cert: %v", err)
	}
	// the CA's claims refuse a user certificate longer than 15 minutes, even if this client were changed
	ca.Now = time.Now
	r := SSHCertRequest{Type: "user", KeyID: "k", Principals: []string{"p"}, Validity: time.Hour}
	if _, err := ca.Sign(ctx, spub, r); err == nil {
		t.Fatal("1 h user certificate signed")
	}
	tok, _ := ca.token(SSHCertRequest{Type: "user", KeyID: "k", Principals: []string{"p"}, Validity: time.Hour}, "https://"+ca.ServerName+"/1.0/ssh/sign")
	if code, msg := rawSign(t, ca, spub, tok); code < 400 {
		t.Fatalf("CA signed a 1 h user certificate: %d %s", code, msg)
	}
	// a token signed with another key is refused
	other, _ := newTestCA(t, url, root)
	if _, err := other.Sign(ctx, spub, SSHCertRequest{Type: "user", KeyID: "k", Principals: []string{"p"}, Validity: time.Minute}); err == nil ||
		!strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("foreign provisioner key: %v", err)
	}
}
