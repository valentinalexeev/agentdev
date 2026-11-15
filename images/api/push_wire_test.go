package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/hkdf"
)

// TestPushWireFormat sends through the real library (RFC 8291 aes128gcm, RFC 8292 VAPID) to a fake push service and
// decrypts the body with the browser's private key: the push service gets only ciphertext, and the plaintext is the
// checked payload.
func TestPushWireFormat(t *testing.T) {
	e := newPushEnv(t)
	ctx := context.Background()
	var got struct {
		enc, auth, ttl string
		body           []byte
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.enc, got.auth, got.ttl = r.Header.Get("Content-Encoding"), r.Header.Get("Authorization"), r.Header.Get("TTL")
		got.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	e.p.send = newPusher(e.store, e.p.w, e.p.ar, e.p.channels).send // the library, not the fake
	e.p.client = &http.Client{}                                     // the test server is on loopback: no address guard here
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	_, _ = rand.Read(secret)
	host := strings.TrimPrefix(srv.URL, "https://")
	ep := "https://localhost/push/1"
	sub := pushSub{Subject: "s-w", Endpoint: ep, P256dh: base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes()),
		Auth: base64.RawURLEncoding.EncodeToString(secret)}
	// route the endpoint host to the test server (its client trusts the test certificate)
	e.p.client.Transport = rewrite{base: srv.Client().Transport.(*http.Transport), to: host}
	e.ch.AllowedHosts = append(e.ch.AllowedHosts, "localhost")
	a, _ := approvalPayload(fromApprovalObject(approvalObj("carol", aid, "owner/x", t0)))
	msg, _ := json.Marshal(a)
	e.p.sendOne(ctx, e.ch, sub, msg, a.Tag)
	if got.enc != "aes128gcm" || !strings.HasPrefix(got.auth, "vapid t=") || got.ttl == "" {
		t.Fatalf("headers: %q %q %q", got.enc, got.auth, got.ttl)
	}
	if strings.Contains(string(got.body), "carol") {
		t.Fatal("body is not encrypted")
	}
	plain := decrypt8291(t, got.body, ua, secret)
	if string(plain) != string(msg) {
		t.Fatalf("decrypted %q", plain)
	}
}

type rewrite struct {
	base *http.Transport
	to   string
}

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Host = r.to
	return r.base.RoundTrip(req)
}

// decrypt8291 is the user agent side of RFC 8291 §3.4 and RFC 8188 (one record).
func decrypt8291(t *testing.T, body []byte, ua *ecdh.PrivateKey, authSecret []byte) []byte {
	t.Helper()
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	if rs < 18 {
		t.Fatalf("record size %d", rs)
	}
	asPub := body[21 : 21+idlen]
	ct := body[21+idlen:]
	as, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := ua.ECDH(as)
	if err != nil {
		t.Fatal(err)
	}
	info := append(append([]byte("WebPush: info\x00"), ua.PublicKey().Bytes()...), asPub...)
	ikm := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, authSecret, info), ikm); err != nil {
		t.Fatal(err)
	}
	cek := make([]byte, 16)
	nonce := make([]byte, 12)
	_, _ = io.ReadFull(hkdf.New(sha256.New, ikm, salt, []byte("Content-Encoding: aes128gcm\x00")), cek)
	_, _ = io.ReadFull(hkdf.New(sha256.New, ikm, salt, []byte("Content-Encoding: nonce\x00")), nonce)
	blk, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(blk)
	p, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	// strip the padding: the last record ends with 0x02 then zeros
	i := len(p) - 1
	for i >= 0 && p[i] == 0 {
		i--
	}
	if i < 0 || p[i] != 2 {
		t.Fatal("bad padding delimiter")
	}
	return p[:i]
}
