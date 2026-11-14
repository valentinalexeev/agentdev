package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// rawSign posts a prepared token (bypassing the client's own checks).
func rawSign(t *testing.T, ca *SSHCA, pub ssh.PublicKey, ott string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"publicKey": pub.Marshal(), "ott": ott})
	resp, err := ca.Client.Post(ca.URL+"/1.0/ssh/sign", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// newTestCA is a client with a fresh, unknown provisioner key.
func newTestCA(t *testing.T, url string, root []byte) (*SSHCA, *ecdsa.PrivateKey) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	enc := func(b *big.Int) string { return base64.RawURLEncoding.EncodeToString(b.FillBytes(make([]byte, 32))) }
	j, _ := json.Marshal(jwk{Kty: "EC", Crv: "P-256", Kid: "test-kid", X: enc(k.X), Y: enc(k.Y), D: enc(k.D)})
	ca, err := newSSHCA(url, "step-ca.agentdev-executors.svc", j, root)
	if err != nil {
		t.Fatal(err)
	}
	return ca, k
}

// fakeStepCA verifies the token like step-ca (ES256, iss, aud, step.ssh) and signs with its own CA keys.
type fakeStepCA struct {
	t        *testing.T
	pub      *ecdsa.PublicKey
	userCA   ssh.Signer
	hostCA   ssh.Signer
	tamper   func(*ssh.Certificate) // change the issued certificate (tests of checkIssued)
	requests int
}

func newFakeStepCA(t *testing.T) (*fakeStepCA, *httptest.Server, *SSHCA) {
	f := &fakeStepCA{t: t}
	mk := func() ssh.Signer {
		_, k, _ := ed25519.GenerateKey(rand.Reader)
		s, _ := ssh.NewSignerFromKey(k)
		return s
	}
	f.userCA, f.hostCA = mk(), mk()
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	root := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	ca, k := newTestCA(t, srv.URL, root)
	ca.Client.Transport.(*http.Transport).TLSClientConfig.ServerName = "example.com" // httptest certificate
	f.pub = &k.PublicKey
	return f, srv, ca
}

func (f *fakeStepCA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/ssh/roots":
		_ = json.NewEncoder(w).Encode(map[string][]string{
			"userKey": {base64.StdEncoding.EncodeToString(f.userCA.PublicKey().Marshal())},
			"hostKey": {base64.StdEncoding.EncodeToString(f.hostCA.PublicKey().Marshal())}})
		return
	case "/1.0/ssh/sign":
	default:
		http.NotFound(w, r)
		return
	}
	f.requests++
	var req struct {
		PublicKey []byte `json:"publicKey"`
		OTT       string `json:"ott"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	parts := strings.Split(req.OTT, ".")
	if len(parts) != 3 {
		http.Error(w, `{"message":"bad token"}`, 401)
		return
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(f.pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		http.Error(w, `{"message":"bad signature"}`, 401)
		return
	}
	pl, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iss  string `json:"iss"`
		Aud  string `json:"aud"`
		Sub  string `json:"sub"`
		Exp  int64  `json:"exp"`
		Step struct {
			SSH struct {
				CertType    string    `json:"certType"`
				KeyID       string    `json:"keyID"`
				Principals  []string  `json:"principals"`
				ValidAfter  time.Time `json:"validAfter"`
				ValidBefore time.Time `json:"validBefore"`
			} `json:"ssh"`
		} `json:"step"`
		Agentdev struct {
			CriticalOptions map[string]string `json:"criticalOptions"`
		} `json:"agentdev"`
	}
	_ = json.Unmarshal(pl, &c)
	if c.Iss != sshCAProvisioner || !strings.HasSuffix(c.Aud, "/1.0/ssh/sign") || c.Sub != c.Step.SSH.KeyID || c.Exp < time.Now().Unix() {
		http.Error(w, `{"message":"bad claims"}`, 401)
		return
	}
	pub, err := ssh.ParsePublicKey(req.PublicKey)
	if err != nil {
		http.Error(w, `{"message":"bad key"}`, 400)
		return
	}
	cert := &ssh.Certificate{Key: pub, KeyId: c.Step.SSH.KeyID, ValidPrincipals: c.Step.SSH.Principals,
		Permissions: ssh.Permissions{CriticalOptions: c.Agentdev.CriticalOptions, Extensions: map[string]string{}}}
	signer := f.userCA
	cert.CertType = ssh.UserCert
	if c.Step.SSH.CertType == "host" {
		signer, cert.CertType = f.hostCA, ssh.HostCert
		cert.ValidAfter, cert.ValidBefore = uint64(time.Now().Unix()), uint64(time.Now().Add(90*24*time.Hour).Unix())
	} else {
		cert.ValidAfter, cert.ValidBefore = uint64(c.Step.SSH.ValidAfter.Unix()), uint64(c.Step.SSH.ValidBefore.Unix())
	}
	if f.tamper != nil {
		f.tamper(cert)
	}
	if err := cert.SignCert(rand.Reader, signer); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"crt": base64.StdEncoding.EncodeToString(cert.Marshal())})
}

func testKey(t *testing.T) (ssh.Signer, ssh.PublicKey) {
	_, k, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return s, s.PublicKey()
}

func TestSSHCASign(t *testing.T) {
	f, _, ca := newFakeStepCA(t)
	ctx := context.Background()
	_, pub := testKey(t)
	c, err := ca.Sign(ctx, pub, SSHCertRequest{Type: "user", KeyID: "executor/r1/abc", Principals: []string{"agentdev-x-apply"},
		Validity: 10 * time.Minute, CriticalOptions: map[string]string{"force-command": "/usr/local/sbin/agentdev-probe"}})
	if err != nil {
		t.Fatal(err)
	}
	if c.KeyId != "executor/r1/abc" || c.CriticalOptions["force-command"] == "" {
		t.Fatalf("cert %+v", c)
	}
	user, host, err := ca.Keys(ctx)
	if err != nil || len(user) != 1 || len(host) != 1 || !strings.HasPrefix(user[0], "ssh-ed25519 ") {
		t.Fatalf("keys %v %v %v", user, host, err)
	}
	// refused before any request
	n := f.requests
	for _, r := range []SSHCertRequest{
		{Type: "user", KeyID: "k", Principals: []string{"p"}, Validity: time.Hour},
		{Type: "user", KeyID: "k", Principals: []string{"p"}},
		{Type: "user", KeyID: "", Principals: []string{"p"}, Validity: time.Minute},
		{Type: "user", KeyID: "k", Validity: time.Minute},
		{Type: "admin", KeyID: "k", Principals: []string{"p"}, Validity: time.Minute},
		{Type: "user", KeyID: "k", Principals: []string{"p"}, Validity: time.Minute, CriticalOptions: map[string]string{"permit-pty": ""}},
	} {
		if _, err := ca.Sign(ctx, pub, r); err == nil {
			t.Errorf("signed %+v", r)
		}
	}
	if f.requests != n {
		t.Fatal("invalid requests reached the CA")
	}
	// what the CA issues is checked
	for name, tm := range map[string]func(*ssh.Certificate){
		"extension":  func(c *ssh.Certificate) { c.Extensions["permit-pty"] = "" },
		"principals": func(c *ssh.Certificate) { c.ValidPrincipals = append(c.ValidPrincipals, "root") },
		"key id":     func(c *ssh.Certificate) { c.KeyId = "other" },
		"lifetime":   func(c *ssh.Certificate) { c.ValidBefore += 3600 * 24 },
		"type":       func(c *ssh.Certificate) { c.CertType = ssh.HostCert },
		"no force":   func(c *ssh.Certificate) { c.CriticalOptions = nil },
	} {
		f.tamper = tm
		if _, err := ca.Sign(ctx, pub, SSHCertRequest{Type: "user", KeyID: "k", Principals: []string{"p"}, Validity: time.Minute,
			CriticalOptions: map[string]string{"force-command": "x"}}); err == nil {
			t.Errorf("tampered certificate (%s) accepted", name)
		}
	}
	f.tamper = nil
	// another provisioner key is refused by the CA
	other, _ := newTestCA(t, ca.URL, nil2(t, ca))
	other.Client = ca.Client
	if _, err := other.Sign(ctx, pub, SSHCertRequest{Type: "user", KeyID: "k", Principals: []string{"p"}, Validity: time.Minute}); err == nil {
		t.Fatal("foreign key signed")
	}
}

// nil2 returns any valid PEM root (the client is replaced afterwards).
func nil2(t *testing.T, _ *SSHCA) []byte {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

func TestNewSSHCARefusesBadKeys(t *testing.T) {
	root := nil2(t, nil)
	for _, j := range []string{`{}`, `{"kty":"RSA"}`, `{"kty":"EC","crv":"P-256","kid":"k","x":"AA","y":"AA"}`, `not json`} {
		if _, err := newSSHCA("https://x", "x", []byte(j), root); err == nil {
			t.Errorf("accepted %s", j)
		}
	}
	_, err := newSSHCA("https://x", "x", []byte(`{"kty":"EC","crv":"P-256","kid":"k","x":"AA","y":"AA","d":"!!"}`), root)
	if err == nil || !strings.Contains(err.Error(), "field d is not base64url") {
		t.Errorf("bad base64 in d: got %v", err)
	}
}
