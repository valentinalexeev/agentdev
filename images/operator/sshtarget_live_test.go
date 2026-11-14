package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// TestSSHEnrolLive enrols a real OpenSSH server with a real step-ca: SSHCA_TEST_DIR/SSHCA_TEST_URL as in
// TestSSHCALive, SSHD_TEST_ADDR (host:port of a fresh tests/e2e/ssh-server container) and SSHD_TEST_APPLY, a command
// that runs stdin as root on that server (docker exec -i <name> sh). The snippet is scripts/ssh-enrol.sh's.
func TestSSHEnrolLive(t *testing.T) {
	dir, url, addr, apply := os.Getenv("SSHCA_TEST_DIR"), os.Getenv("SSHCA_TEST_URL"), os.Getenv("SSHD_TEST_ADDR"), os.Getenv("SSHD_TEST_APPLY")
	if dir == "" || url == "" || addr == "" || apply == "" {
		t.Skip("SSHCA_TEST_* / SSHD_TEST_* not set")
	}
	prov, _ := os.ReadFile(filepath.Join(dir, "provisioner.json"))
	root, _ := os.ReadFile(filepath.Join(dir, "root_ca.crt"))
	ca, err := newSSHCA(url, "step-ca.agentdev-executors.svc", prov, root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tgt := &v1.SSHTarget{ObjectMeta: metav1.ObjectMeta{Name: "srv", Namespace: "agentdev-executors", Generation: 1},
		Spec: v1.SSHTargetSpec{Host: "srv.example", Executor: "web"}}
	c, e, _, _ := sshTargetFixture(t, tgt)
	e.Dial = func(ctx context.Context, n, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, addr)
	}
	key, err := e.scanHostKey(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	var o v1.SSHTarget
	_ = c.Get(ctx, client.ObjectKeyFromObject(tgt), &o)
	o.Spec.HostKeyFingerprint = ssh.FingerprintSHA256(key)
	_ = c.Update(ctx, &o)
	_ = c.Get(ctx, client.ObjectKeyFromObject(tgt), &o)
	_ = e.Enrol(ctx, ca, &o)
	if o.Status.HostCertificate == "" {
		t.Fatalf("not signed: %+v", o.Status)
	}
	// the admin step: scripts/ssh-enrol.sh with this status, applied as root on the server
	raw, _ := jsonOf(&o)
	fake := t.TempDir()
	_ = os.WriteFile(filepath.Join(fake, "kubectl"), []byte("#!/bin/sh\ncat <<'J'\n"+raw+"\nJ\n"), 0o755)
	gen := exec.Command("bash", "../../scripts/ssh-enrol.sh", "srv")
	gen.Env = append(os.Environ(), "PATH="+fake+":"+os.Getenv("PATH"))
	snippet, err := gen.Output()
	if err != nil {
		t.Fatalf("ssh-enrol.sh: %v", err)
	}
	run := exec.Command("sh", "-c", apply)
	run.Stdin = strings.NewReader(string(snippet))
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("apply snippet: %v\n%s", err, out)
	}
	time.Sleep(time.Second)
	if err := e.Enrol(ctx, ca, &o); err != nil || o.Status.Phase != "Ready" {
		t.Fatalf("probe: %v %+v", err, o.Status)
	}
	if !strings.Contains(o.Status.Message, "user=agentdev-check") {
		t.Fatalf("probe output %q", o.Status.Message)
	}
	hostCA, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(o.Status.HostCAKey))
	login := func(user, principal string, crit map[string]string) (string, error) {
		_, signer, cert, err := newCertKey(ctx, ca, SSHCertRequest{Type: "user", KeyID: "test/" + principal,
			Principals: []string{principal}, Validity: time.Minute, CriticalOptions: crit})
		if err != nil {
			return "", err
		}
		cl, err := e.sshSession(ctx, addr, "srv.example", user, signer, cert, hostCA)
		if err != nil {
			return "", err
		}
		defer cl.Close()
		s, _ := cl.NewSession()
		defer s.Close()
		out, err := s.CombinedOutput("id -un; sudo -n true && echo SUDO || echo NOSUDO")
		return string(out), err
	}
	// apply principal: user agentdev with sudo; check principal: agentdev-check without sudo
	if out, err := login("agentdev", "agentdev-web-apply", nil); err != nil || !strings.Contains(out, "SUDO") || strings.Contains(out, "NOSUDO") {
		t.Fatalf("apply login: %v %q", err, out)
	}
	if out, err := login("agentdev-check", "agentdev-web-check", nil); err != nil || !strings.Contains(out, "NOSUDO") {
		t.Fatalf("check login: %v %q", err, out)
	}
	// refused: the check principal as the apply user, another executor's principal, root, the probe principal
	// without its force-command running something else
	for _, c := range [][2]string{{"agentdev", "agentdev-web-check"}, {"agentdev", "agentdev-other-apply"}, {"root", "agentdev-web-apply"},
		{"agentdev-check", "agentdev-other-check"}} {
		if _, err := login(c[0], c[1], nil); err == nil {
			t.Errorf("login %s with %s accepted", c[0], c[1])
		}
	}
	if out, err := login("agentdev-check", "agentdev-probe", map[string]string{"force-command": sshProbeCommand}); err != nil ||
		!strings.HasPrefix(out, sshProbeMarker) {
		t.Fatalf("probe principal ran %q (%v)", out, err)
	}
}
