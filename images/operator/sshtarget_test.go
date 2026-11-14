package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// testSSHD is a minimal sshd: user certificates of userCA for the principals of each user (AuthorizedPrincipalsFile),
// force-command honoured, presenting hostCert when set. It records the key IDs it accepted.
type testSSHD struct {
	addr     string
	hostKey  ssh.Signer
	hostCert *ssh.Certificate
	userCA   ssh.PublicKey
	users    map[string][]string // user -> principals
	mu       sync.Mutex
	keyIDs   []string
	commands []string
}

func newTestSSHD(t *testing.T, userCA ssh.PublicKey) *testSSHD {
	_, k, _ := ed25519.GenerateKey(rand.Reader)
	hk, _ := ssh.NewSignerFromKey(k)
	s := &testSSHD{hostKey: hk, userCA: userCA, users: map[string][]string{
		"agentdev-check": {"agentdev-probe", "agentdev-x-check"}, "agentdev": {"agentdev-x-apply"}}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s.addr = ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *testSSHD) serve(c net.Conn) {
	defer c.Close()
	checker := &ssh.CertChecker{IsUserAuthority: func(a ssh.PublicKey) bool { return bytes.Equal(a.Marshal(), s.userCA.Marshal()) },
		SupportedCriticalOptions: []string{"force-command", "source-address"}}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(m ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		cert, ok := k.(*ssh.Certificate)
		if !ok {
			return nil, fmt.Errorf("certificates only")
		}
		for _, p := range s.users[m.User()] {
			if p2 := checker.CheckCert(p, cert); p2 == nil {
				s.mu.Lock()
				s.keyIDs = append(s.keyIDs, cert.KeyId)
				s.mu.Unlock()
				return &ssh.Permissions{CriticalOptions: cert.CriticalOptions}, nil
			}
		}
		return nil, fmt.Errorf("no principal for %s", m.User())
	}}
	if s.hostCert != nil {
		cs, _ := ssh.NewCertSigner(s.hostCert, s.hostKey)
		cfg.AddHostKey(cs)
	}
	cfg.AddHostKey(s.hostKey)
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, creqs, _ := nc.Accept()
		go func() {
			for r := range creqs {
				if r.Type == "exec" {
					cmd := string(r.Payload[4:])
					if fc := sc.Permissions.CriticalOptions["force-command"]; fc != "" {
						cmd = fc
					}
					s.mu.Lock()
					s.commands = append(s.commands, cmd)
					s.mu.Unlock()
					_ = r.Reply(true, nil)
					if cmd == sshProbeCommand {
						fmt.Fprintf(ch, "%s user=%s\n", sshProbeMarker, sc.User())
					}
					_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
					ch.Close()
					return
				}
				_ = r.Reply(false, nil)
			}
		}()
	}
}

func sshTargetFixture(t *testing.T, objs ...client.Object) (client.Client, *SSHEnroller, *fakeStepCA, *SSHCA) {
	sch := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = v1.AddToScheme(sch)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).WithStatusSubresource(&v1.SSHTarget{}).Build()
	f, _, ca := newFakeStepCA(t)
	e := &SSHEnroller{Client: c, Direct: c, NS: "agentdev-executors", CA: func(context.Context) (*SSHCA, error) { return ca, nil },
		Poll: time.Minute, Probe: 10 * time.Minute, Now: time.Now}
	return c, e, f, ca
}

func TestSSHTargetEnrolment(t *testing.T) {
	var srv *testSSHD
	tgt := &v1.SSHTarget{ObjectMeta: metav1.ObjectMeta{Name: "web1", Namespace: "agentdev-executors", Generation: 1},
		Spec: v1.SSHTargetSpec{Host: "web1.example", Executor: "x"}}
	c, e, f, _ := sshTargetFixture(t, tgt)
	srv = newTestSSHD(t, f.userCA.PublicKey())
	host, port, _ := net.SplitHostPort(srv.addr)
	_ = host
	e.Dial = func(ctx context.Context, n, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, srv.addr)
	}
	get := func() *v1.SSHTarget {
		var o v1.SSHTarget
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(tgt), &o); err != nil {
			t.Fatal(err)
		}
		return &o
	}
	_ = port

	// wrong fingerprint: nothing signed
	o := get()
	o.Spec.HostKeyFingerprint = "SHA256:" + strings.Repeat("A", 43)
	_ = c.Update(context.Background(), o)
	n := f.requests
	e.PollAll(context.Background())
	o = get()
	if o.Status.Phase != "HostKeyMismatch" || o.Status.HostCertificate != "" || f.requests != n {
		t.Fatalf("mismatch: %+v (sign calls %d)", o.Status, f.requests-n)
	}
	if o.Status.ObservedFingerprint != ssh.FingerprintSHA256(srv.hostKey.PublicKey()) {
		t.Fatalf("observed %s", o.Status.ObservedFingerprint)
	}

	// right fingerprint, server not yet configured (no host certificate): signed, probe refuses the plain host key
	o.Spec.HostKeyFingerprint = ssh.FingerprintSHA256(srv.hostKey.PublicKey())
	_ = c.Update(context.Background(), o)
	e.PollAll(context.Background())
	o = get()
	if o.Status.Phase != "Enrolling" || o.Status.HostCertificate == "" || len(o.Status.UserCAKeys) != 1 || o.Status.HostCAKey == "" {
		t.Fatalf("enrolling: %+v", o.Status)
	}
	if !strings.Contains(o.Status.Message, "probe failed") {
		t.Fatalf("message %q", o.Status.Message)
	}
	// the admin applies the snippet: the server now presents the certificate
	pk, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(o.Status.HostCertificate))
	srv.hostCert = pk.(*ssh.Certificate)
	e.PollAll(context.Background())
	o = get()
	if o.Status.Phase != "Ready" || !strings.HasPrefix(o.Status.Message, sshProbeMarker+" user=agentdev-check") {
		t.Fatalf("ready: %+v", o.Status)
	}
	srv.mu.Lock()
	if got := srv.keyIDs[len(srv.keyIDs)-1]; got != "probe/web1" || srv.commands[len(srv.commands)-1] != sshProbeCommand {
		t.Fatalf("key id %q cmds %v", got, srv.commands)
	}
	srv.mu.Unlock()
	// the host certificate is kept (no new signature) while valid
	n = f.requests
	cert1 := o.Status.HostCertificate
	o.Status.LastProbe = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	_ = c.Status().Update(context.Background(), o)
	e.PollAll(context.Background())
	o = get()
	if o.Status.HostCertificate != cert1 || f.requests != n+1 { // only the probe certificate
		t.Fatalf("host certificate re-signed (calls %d)", f.requests-n)
	}
	// a host certificate of another CA is refused: NotReady
	_, k2, _ := ed25519.GenerateKey(rand.Reader)
	rogue, _ := ssh.NewSignerFromKey(k2)
	bad := *srv.hostCert
	bad.Signature = nil
	_ = bad.SignCert(rand.Reader, rogue)
	srv.hostCert = &bad
	o.Status.LastProbe = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	_ = c.Status().Update(context.Background(), o)
	e.PollAll(context.Background())
	if o = get(); o.Status.Phase != "NotReady" {
		t.Fatalf("rogue host CA: %+v", o.Status)
	}
	// the server swaps its host key: mismatch, certificate dropped
	_, k3, _ := ed25519.GenerateKey(rand.Reader)
	srv.hostKey, _ = ssh.NewSignerFromKey(k3)
	srv.hostCert = nil
	e.PollAll(context.Background())
	if o = get(); o.Status.Phase != "HostKeyMismatch" || o.Status.HostCertificate != "" {
		t.Fatalf("swapped key: %+v", o.Status)
	}
}

func TestSSHTargetNoCA(t *testing.T) {
	tgt := &v1.SSHTarget{ObjectMeta: metav1.ObjectMeta{Name: "web1", Namespace: "agentdev-executors"},
		Spec: v1.SSHTargetSpec{Host: "web1.example", Executor: "x", HostKeyFingerprint: "SHA256:" + strings.Repeat("A", 43)}}
	c, e, _, _ := sshTargetFixture(t, tgt)
	e.CA = func(context.Context) (*SSHCA, error) { return nil, fmt.Errorf("Secret step-ca-provisioner not found") }
	e.PollAll(context.Background())
	var o v1.SSHTarget
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(tgt), &o)
	if o.Status.Phase != "Pending" || !strings.Contains(o.Status.Message, "step-ca-provisioner") {
		t.Fatalf("%+v", o.Status)
	}
}

func jsonOf(o any) (string, error) {
	b, err := json.Marshal(o)
	return string(b), err
}
