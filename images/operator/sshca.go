package main

// SSH certificate authority client (#204, docs/adr/0007). The operator is the only requester of step-ca in EXEC_NS:
// it signs a one-time token (JWT, ES256) with the agentdev-operator JWK provisioner key (Secret step-ca-provisioner)
// and posts it with the public key to /1.0/ssh/sign. The CA's template takes principals and key ID from the token and
// adds no extensions; critical options (force-command, source-address) come only from the token's agentdev claim.
// The token is signed here, so nothing in a Job can ask for a certificate.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	sshCAProvisioner       = "agentdev-operator"
	sshCAProvisionerSecret = "step-ca-provisioner"
	sshUserCertMax         = 15 * time.Minute
)

// jwk is the private JWK that `step crypto jwk create --kty EC --crv P-256` writes.
type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Kid string `json:"kid"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d"`
}

// SSHCA signs SSH certificates with step-ca.
type SSHCA struct {
	URL        string // https://step-ca.<ns>.svc:9000
	ServerName string // a dnsName of the CA (TLS name and token audience)
	Name       string // provisioner name
	Kid        string
	key        *ecdsa.PrivateKey
	Client     *http.Client
	Now        func() time.Time
}

// newSSHCA builds the client from the provisioner JWK and the CA root certificate (PEM). serverName is the name in
// the CA's TLS certificate (step-ca.<ns>.svc); url may point elsewhere (tests).
func newSSHCA(url, serverName string, provisionerJSON, rootPEM []byte) (*SSHCA, error) {
	var k jwk
	if err := json.Unmarshal(provisionerJSON, &k); err != nil {
		return nil, fmt.Errorf("provisioner key: %w", err)
	}
	if k.Kty != "EC" || k.Crv != "P-256" || k.D == "" || k.Kid == "" {
		return nil, errors.New("provisioner key: want a private EC P-256 JWK with kid")
	}
	var decErr error
	b := func(field, s string) *big.Int {
		v, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil && decErr == nil {
			decErr = fmt.Errorf("provisioner key: field %s is not base64url: %w", field, err)
		}
		return new(big.Int).SetBytes(v)
	}
	priv := &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: b("x", k.X), Y: b("y", k.Y)}, D: b("d", k.D)}
	if decErr != nil {
		return nil, decErr
	}
	if !priv.Curve.IsOnCurve(priv.X, priv.Y) {
		return nil, errors.New("provisioner key: point not on curve")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(rootPEM) {
		return nil, errors.New("CA root certificate: no PEM certificate")
	}
	tr := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12}}
	return &SSHCA{URL: strings.TrimRight(url, "/"), ServerName: serverName, Name: sshCAProvisioner, Kid: k.Kid, key: priv,
		Client: &http.Client{Transport: tr, Timeout: 20 * time.Second}, Now: time.Now}, nil
}

// SSHCertRequest is one certificate.
type SSHCertRequest struct {
	Type            string // "user" or "host"
	KeyID           string // executor/<run>/<commit>, probe/<target>, host/<target>
	Principals      []string
	Validity        time.Duration
	CriticalOptions map[string]string // force-command, source-address (user certificates)
}

func (r SSHCertRequest) check() error {
	if r.Type != "user" && r.Type != "host" {
		return fmt.Errorf("certificate type %q", r.Type)
	}
	if r.KeyID == "" || len(r.Principals) == 0 {
		return errors.New("key ID and principals are required")
	}
	if r.Type == "user" && (r.Validity <= 0 || r.Validity > sshUserCertMax) {
		return fmt.Errorf("user certificate validity %s: 1s to %s", r.Validity, sshUserCertMax)
	}
	for k := range r.CriticalOptions {
		if k != "force-command" && k != "source-address" {
			return fmt.Errorf("critical option %q is not allowed", k)
		}
	}
	return nil
}

// token is the one-time token for one /1.0/ssh/sign call.
func (c *SSHCA) token(r SSHCertRequest, aud string) (string, error) {
	now := c.Now()
	var jti [16]byte
	if _, err := rand.Read(jti[:]); err != nil {
		return "", err
	}
	ssh := map[string]any{"certType": r.Type, "keyID": r.KeyID, "principals": r.Principals}
	if r.Validity > 0 {
		ssh["validAfter"] = now.Add(-time.Minute).UTC().Format(time.RFC3339)
		ssh["validBefore"] = now.Add(r.Validity).UTC().Format(time.RFC3339)
	}
	claims := map[string]any{"iss": c.Name, "sub": r.KeyID, "aud": aud, "iat": now.Unix(), "nbf": now.Add(-30 * time.Second).Unix(),
		"exp": now.Add(5 * time.Minute).Unix(), "jti": hex.EncodeToString(jti[:]), "step": map[string]any{"ssh": ssh}}
	if len(r.CriticalOptions) > 0 {
		claims["agentdev"] = map[string]any{"criticalOptions": r.CriticalOptions}
	}
	return signES256(c.key, c.Kid, claims)
}

func signES256(key *ecdsa.PrivateKey, kid string, claims any) (string, error) {
	h, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": kid, "typ": "JWT"})
	p, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	in := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
	sum := sha256.Sum256([]byte(in))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return in + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Sign returns the certificate for pub. The result is checked: type, key, key ID and principals as requested.
func (c *SSHCA) Sign(ctx context.Context, pub ssh.PublicKey, r SSHCertRequest) (*ssh.Certificate, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	ott, err := c.token(r, "https://"+c.ServerName+"/1.0/ssh/sign") // step-ca checks the audience against its dnsNames
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"publicKey": pub.Marshal(), "ott": ott, "certType": r.Type, "keyID": r.KeyID,
		"principals": r.Principals})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/1.0/ssh/sign", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("step-ca: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return nil, fmt.Errorf("step-ca: HTTP %d %s", resp.StatusCode, e.Message)
	}
	var out struct {
		Crt string `json:"crt"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("step-ca answer: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(out.Crt)
	if err != nil {
		return nil, fmt.Errorf("step-ca certificate: %w", err)
	}
	pk, err := ssh.ParsePublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("step-ca certificate: %w", err)
	}
	cert, ok := pk.(*ssh.Certificate)
	if !ok {
		return nil, errors.New("step-ca answer is not a certificate")
	}
	return cert, checkIssued(cert, pub, r)
}

// checkIssued: the CA gave exactly what was asked (fail closed if its template or claims were changed).
func checkIssued(c *ssh.Certificate, pub ssh.PublicKey, r SSHCertRequest) error {
	want := uint32(ssh.UserCert)
	if r.Type == "host" {
		want = ssh.HostCert
	}
	switch {
	case c.CertType != want:
		return errors.New("issued certificate: wrong type")
	case !bytes.Equal(c.Key.Marshal(), pub.Marshal()):
		return errors.New("issued certificate: wrong key")
	case c.KeyId != r.KeyID:
		return fmt.Errorf("issued certificate: key ID %q", c.KeyId)
	case strings.Join(c.ValidPrincipals, ",") != strings.Join(r.Principals, ","):
		return fmt.Errorf("issued certificate: principals %v", c.ValidPrincipals)
	case r.Type == "user" && len(c.Extensions) != 0:
		return fmt.Errorf("issued certificate: extensions %v", c.Extensions)
	case r.Type == "user" && time.Duration(c.ValidBefore-c.ValidAfter)*time.Second > sshUserCertMax+2*time.Minute:
		return errors.New("issued certificate: valid too long")
	}
	for k, v := range r.CriticalOptions {
		if c.CriticalOptions[k] != v {
			return fmt.Errorf("issued certificate: critical option %s missing", k)
		}
	}
	return nil
}

// SSHCAKeys returns the user and host CA public keys (authorized_keys form) from /ssh/roots.
func (c *SSHCA) Keys(ctx context.Context) (user []string, host []string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/ssh/roots", nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("step-ca: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("step-ca /ssh/roots: HTTP %d", resp.StatusCode)
	}
	var out struct {
		User []string `json:"userKey"`
		Host []string `json:"hostKey"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, nil, err
	}
	conv := func(in []string) ([]string, error) {
		var o []string
		for _, s := range in {
			raw, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				return nil, err
			}
			k, err := ssh.ParsePublicKey(raw)
			if err != nil {
				return nil, err
			}
			o = append(o, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))))
		}
		return o, nil
	}
	if user, err = conv(out.User); err != nil {
		return nil, nil, err
	}
	if host, err = conv(out.Host); err != nil {
		return nil, nil, err
	}
	if len(user) == 0 || len(host) == 0 {
		return nil, nil, errors.New("step-ca has no SSH CA keys")
	}
	return user, host, nil
}
