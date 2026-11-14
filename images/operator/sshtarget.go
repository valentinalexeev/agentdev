package main

// Server enrolment (#204, docs/adr/0007). For every SSHTarget in EXEC_NS the operator
//
//  1. reads the server's ed25519 host key over SSH and compares its fingerprint with spec.hostKeyFingerprint (written
//     by an admin who read it on the server). A mismatch stops here (HostKeyMismatch) and is never signed.
//  2. asks step-ca for a host certificate of that key (principal spec.host, key ID host/<target>) and stores it in
//     the status with the CA keys, for scripts/ssh-enrol.sh. Renewed 30 days before it expires.
//  3. probes: a fresh key with a 2-minute user certificate (principal agentdev-probe, force-command
//     /usr/local/sbin/agentdev-probe), as spec.checkUser, accepting the server only with a host certificate of the
//     host CA (no trust on first use). Ready only if the probe prints its marker. Repeated every probe interval.
//
// The operator connects to the servers itself (its pod has no egress policy); executor Jobs reach them only through
// the executor egress proxy (executor_egress.go).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups=agentdev.dev,resources=sshtargets,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentdev.dev,resources=sshtargets/status,verbs=get;update;patch

const (
	sshProbeCommand   = "/usr/local/sbin/agentdev-probe"
	sshProbePrincipal = "agentdev-probe"
	sshProbeMarker    = "agentdev-probe ok"
	sshHostRenew      = 30 * 24 * time.Hour
)

// SSHEnroller enrols SSHTargets. A nil CA (no Secret step-ca-provisioner) leaves every target Pending.
type SSHEnroller struct {
	client.Client
	Direct client.Reader
	NS     string
	CA     func(ctx context.Context) (*SSHCA, error)
	Poll   time.Duration // pass interval
	Probe  time.Duration // probe interval of Ready targets
	Dial   func(ctx context.Context, network, addr string) (net.Conn, error)
	Now    func() time.Time
}

// Start implements manager.Runnable.
func (e *SSHEnroller) Start(ctx context.Context) error {
	t := time.NewTicker(e.Poll)
	defer t.Stop()
	for {
		e.PollAll(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// PollAll handles every SSHTarget once; one target's failure does not stop the others.
func (e *SSHEnroller) PollAll(ctx context.Context) {
	var list v1.SSHTargetList
	if err := e.Direct.List(ctx, &list, client.InNamespace(e.NS)); err != nil {
		log.FromContext(ctx).Error(err, "list ssh targets")
		return
	}
	if len(list.Items) == 0 {
		return
	}
	ca, err := e.CA(ctx)
	for i := range list.Items {
		t := &list.Items[i]
		if err != nil {
			e.setStatus(ctx, t, "Pending", "CAUnavailable", "SSH CA: "+err.Error(), false)
			continue
		}
		if err := e.Enrol(ctx, ca, t); err != nil {
			log.FromContext(ctx).V(1).Info("ssh target", "target", t.Name, "err", err.Error())
		}
	}
}

func (e *SSHEnroller) addr(t *v1.SSHTarget) string {
	p := t.Spec.Port
	if p == 0 {
		p = 22
	}
	return net.JoinHostPort(t.Spec.Host, strconv.Itoa(int(p)))
}

func checkUser(t *v1.SSHTarget) string {
	if t.Spec.CheckUser == "" {
		return "agentdev-check"
	}
	return t.Spec.CheckUser
}

// Enrol runs the steps above for one target and writes its status.
func (e *SSHEnroller) Enrol(ctx context.Context, ca *SSHCA, t *v1.SSHTarget) error {
	now := e.Now()
	userKeys, hostKeys, err := ca.Keys(ctx)
	if err != nil {
		e.setStatus(ctx, t, "Pending", "CAUnavailable", "SSH CA: "+err.Error(), false)
		return err
	}
	t.Status.UserCAKeys, t.Status.HostCAKey = userKeys, hostKeys[0]
	hostCA, _, _, _, err := ssh.ParseAuthorizedKey([]byte(hostKeys[0]))
	if err != nil {
		return err
	}
	// 1. host key, compared with the admin's fingerprint
	hostKey, err := e.scanHostKey(ctx, e.addr(t))
	if err != nil {
		e.setStatus(ctx, t, notReadyOr(t, "Pending"), "Unreachable", "read host key: "+err.Error(), false)
		return err
	}
	fp := ssh.FingerprintSHA256(hostKey)
	t.Status.ObservedFingerprint = fp
	if fp != t.Spec.HostKeyFingerprint {
		t.Status.HostCertificate, t.Status.HostCertificateNotAfter = "", nil
		e.setStatus(ctx, t, "HostKeyMismatch", "HostKeyMismatch",
			fmt.Sprintf("the server presents %s, spec.hostKeyFingerprint is %s: not signed", fp, t.Spec.HostKeyFingerprint), false)
		return errors.New("host key mismatch")
	}
	// 2. host certificate (kept while it is for this key and not near its end)
	if !hostCertValid(t, hostKey, now) {
		c, err := ca.Sign(ctx, hostKey, SSHCertRequest{Type: "host", KeyID: "host/" + t.Name, Principals: []string{t.Spec.Host}})
		if err != nil {
			e.setStatus(ctx, t, notReadyOr(t, "Pending"), "SignFailed", "host certificate: "+err.Error(), false)
			return err
		}
		t.Status.HostCertificate = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(c)))
		t.Status.HostCertificateNotAfter = &metav1.Time{Time: time.Unix(int64(c.ValidBefore), 0)}
	}
	// 3. probe (Ready targets only every probe interval)
	if t.Status.Phase == "Ready" && t.Status.LastProbe != nil && now.Sub(t.Status.LastProbe.Time) < e.Probe &&
		readyGen(t) == t.Generation {
		return e.writeStatus(ctx, t)
	}
	t.Status.LastProbe = &metav1.Time{Time: now}
	out, err := e.probe(ctx, ca, t, hostCA)
	if err != nil {
		e.setStatus(ctx, t, notReadyOr(t, "Enrolling"), "ProbeFailed",
			"probe failed (apply the snippet of scripts/ssh-enrol.sh "+t.Name+" on the server): "+err.Error(), false)
		return err
	}
	e.setStatus(ctx, t, "Ready", "ProbeSucceeded", strings.TrimSpace(out), true)
	return nil
}

func wasReady(t *v1.SSHTarget) bool {
	c := meta.FindStatusCondition(t.Status.Conditions, "Ready")
	return c != nil && c.Reason == "ProbeSucceeded"
}

func readyGen(t *v1.SSHTarget) int64 {
	if c := meta.FindStatusCondition(t.Status.Conditions, "Ready"); c != nil {
		return c.ObservedGeneration
	}
	return -1
}

func notReadyOr(t *v1.SSHTarget, def string) string {
	if wasReady(t) || t.Status.Phase == "NotReady" {
		return "NotReady"
	}
	return def
}

func hostCertValid(t *v1.SSHTarget, hostKey ssh.PublicKey, now time.Time) bool {
	if t.Status.HostCertificate == "" || t.Status.HostCertificateNotAfter == nil || t.Status.HostCertificateNotAfter.Sub(now) < sshHostRenew {
		return false
	}
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(t.Status.HostCertificate))
	if err != nil {
		return false
	}
	c, ok := pk.(*ssh.Certificate)
	return ok && bytes.Equal(c.Key.Marshal(), hostKey.Marshal()) && len(c.ValidPrincipals) == 1 && c.ValidPrincipals[0] == t.Spec.Host
}

func (e *SSHEnroller) dialer() func(ctx context.Context, network, addr string) (net.Conn, error) {
	if e.Dial != nil {
		return e.Dial
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext
}

// errHostKey stops the scan handshake once the key is known.
var errHostKey = errors.New("host key read")

// scanHostKey reads the server's plain ed25519 host key (not its certificate).
func (e *SSHEnroller) scanHostKey(ctx context.Context, addr string) (ssh.PublicKey, error) {
	conn, err := e.dialer()(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	var key ssh.PublicKey
	cfg := &ssh.ClientConfig{User: "agentdev-scan", HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error { key = k; return errHostKey }, Timeout: 15 * time.Second}
	_, _, _, err = ssh.NewClientConn(conn, addr, cfg)
	if key != nil {
		return key, nil
	}
	if err == nil {
		err = errors.New("no host key")
	}
	return nil, err
}

// sshSession opens a connection as user with key and cert; the server must present a host certificate of hostCA
// for host. Shared by the probe and the executor's ProxyCommand-free checks.
func (e *SSHEnroller) sshSession(ctx context.Context, addr, host, user string, signer ssh.Signer, cert *ssh.Certificate,
	hostCA ssh.PublicKey) (*ssh.Client, error) {
	cs, err := ssh.NewCertSigner(cert, signer)
	if err != nil {
		return nil, err
	}
	checker := &ssh.CertChecker{
		IsHostAuthority: func(auth ssh.PublicKey, _ string) bool { return bytes.Equal(auth.Marshal(), hostCA.Marshal()) },
		Clock:           e.Now,
	}
	cfg := &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(cs)},
		HostKeyAlgorithms: []string{ssh.CertAlgoED25519v01},
		HostKeyCallback: func(_ string, remote net.Addr, k ssh.PublicKey) error {
			// the principal is spec.host, whatever address the connection went to
			return checker.CheckHostKey(net.JoinHostPort(host, "22"), remote, k)
		},
		Timeout: 15 * time.Second}
	conn, err := e.dialer()(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// newCertKey makes a fresh ed25519 key and has the CA sign it.
func newCertKey(ctx context.Context, ca *SSHCA, r SSHCertRequest) (ed25519.PrivateKey, ssh.Signer, *ssh.Certificate, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, nil, nil, err
	}
	cert, err := ca.Sign(ctx, signer.PublicKey(), r)
	if err != nil {
		return nil, nil, nil, err
	}
	return priv, signer, cert, nil
}

func (e *SSHEnroller) probe(ctx context.Context, ca *SSHCA, t *v1.SSHTarget, hostCA ssh.PublicKey) (string, error) {
	_, signer, cert, err := newCertKey(ctx, ca, SSHCertRequest{Type: "user", KeyID: "probe/" + t.Name,
		Principals: []string{sshProbePrincipal}, Validity: 2 * time.Minute,
		CriticalOptions: map[string]string{"force-command": sshProbeCommand}})
	if err != nil {
		return "", err
	}
	c, err := e.sshSession(ctx, e.addr(t), t.Spec.Host, checkUser(t), signer, cert, hostCA)
	if err != nil {
		return "", err
	}
	defer c.Close()
	s, err := c.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	var out bytes.Buffer
	s.Stdout = &limitWriter{w: &out, n: 4096}
	if err := s.Run("probe"); err != nil { // the command is replaced by force-command
		return "", fmt.Errorf("probe command: %w", err)
	}
	line := strings.TrimSpace(out.String())
	if !strings.HasPrefix(line, sshProbeMarker) {
		return "", fmt.Errorf("unexpected probe output %q", truncate(line, 120))
	}
	return truncate(line, 200), nil
}

type limitWriter struct {
	w interface{ Write([]byte) (int, error) }
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if len(q) > l.n {
		q = q[:l.n]
	}
	l.n -= len(q)
	_, err := l.w.Write(q)
	return len(p), err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (e *SSHEnroller) setStatus(ctx context.Context, t *v1.SSHTarget, phase, reason, msg string, ready bool) {
	t.Status.Phase, t.Status.Message = phase, truncate(msg, 1000)
	st := metav1.ConditionFalse
	if ready {
		st = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{Type: "Ready", Status: st, Reason: reason,
		Message: truncate(msg, 1000), ObservedGeneration: t.Generation})
	if err := e.writeStatus(ctx, t); err != nil {
		log.FromContext(ctx).Error(err, "ssh target status", "target", t.Name)
	}
}

func (e *SSHEnroller) writeStatus(ctx context.Context, t *v1.SSHTarget) error {
	return e.Status().Update(ctx, t)
}

// newSSHEnroller is the enroller of main: SSH_ENROL_POLL_SECONDS (60) per pass, SSH_PROBE_MINUTES (10) per probe.
func newSSHEnroller(c client.Client, r client.Reader, ns string) *SSHEnroller {
	return &SSHEnroller{Client: c, Direct: r, NS: ns, CA: sshCAFromSecret(r, ns), Now: time.Now,
		Poll:  time.Duration(envInt("SSH_ENROL_POLL_SECONDS", 60)) * time.Second,
		Probe: time.Duration(envInt("SSH_PROBE_MINUTES", 10)) * time.Minute}
}

// sshCAFromSecret builds the CA client from Secret step-ca-provisioner in ns (read on every pass: rotation needs no
// restart).
func sshCAFromSecret(r client.Reader, ns string) func(ctx context.Context) (*SSHCA, error) {
	return func(ctx context.Context) (*SSHCA, error) {
		var s corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: sshCAProvisionerSecret}, &s); err != nil {
			return nil, fmt.Errorf("Secret %s: %w (run scripts/ssh-ca-init.sh)", sshCAProvisionerSecret, err)
		}
		name := "step-ca." + ns + ".svc"
		return newSSHCA("https://"+name+":9000", name, s.Data["provisioner.json"], s.Data["root_ca.crt"])
	}
}
