package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// TestAnsibleStagesLive runs real ansible-playbook (ANSIBLE_TEST_IMAGE, e.g. alpine/ansible) as uid 1000 with the
// exact files of a run Secret (sshRunFiles) against an enrolled OpenSSH server (as TestSSHEnrolLive leaves it):
// the check stage cannot change the server even with check_mode: false and cannot sudo, the apply stage can, and
// sshd logs the run's key ID. SSHD_TEST_NET is the docker network/host the Ansible container reaches the server on
// (SSHD_TEST_TARGET host:port inside that network). Skipped unless all are set.
func TestAnsibleStagesLive(t *testing.T) {
	dir, url, img := os.Getenv("SSHCA_TEST_DIR"), os.Getenv("SSHCA_TEST_URL"), os.Getenv("ANSIBLE_TEST_IMAGE")
	netw, target, apply := os.Getenv("SSHD_TEST_NET"), os.Getenv("SSHD_TEST_TARGET"), os.Getenv("SSHD_TEST_APPLY")
	if dir == "" || url == "" || img == "" || netw == "" || target == "" || apply == "" {
		t.Skip("SSHCA_TEST_* / ANSIBLE_TEST_IMAGE / SSHD_TEST_* not set")
	}
	prov, _ := os.ReadFile(filepath.Join(dir, "provisioner.json"))
	root, _ := os.ReadFile(filepath.Join(dir, "root_ca.crt"))
	ca, err := newSSHCA(url, "step-ca.agentdev-executors.svc", prov, root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, hostKeys, err := ca.Keys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(target)
	tg := v1.SSHTarget{ObjectMeta: metav1.ObjectMeta{Name: "srv"}, Spec: v1.SSHTargetSpec{Host: "srv.example", Executor: "web"},
		Status: v1.SSHTargetStatus{Phase: "Ready", HostCAKey: hostKeys[0]}}
	if port != "22" {
		t.Fatal("SSHD_TEST_TARGET must use port 22 (the host certificate is for srv.example)")
	}
	// the playbook: a marker written with check_mode: false (runs for real even under --check) and a sudo task
	play := `- hosts: all
  gather_facts: false
  tasks:
    - name: marker that ignores check mode
      ansible.builtin.command: touch /tmp/agentdev-marker-{{ stage }}
      check_mode: false
    - name: root file
      become: true
      ansible.builtin.copy: {dest: /etc/agentdev-applied, content: "{{ stage }}\n"}
`
	run := func(stage, principal, user string, check bool) (string, error) {
		priv, _, cert, err := newCertKey(ctx, ca, SSHCertRequest{Type: "user", KeyID: "executor/e2e-" + stage + "/" + strings.Repeat("a", 40),
			Principals: []string{principal}, Validity: 5 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		files, err := sshRunFiles([]v1.SSHTarget{tg}, user, priv, cert)
		if err != nil {
			t.Fatal(err)
		}
		// direct connection inside the docker network instead of connect-proxy (the kind e2e covers the proxy)
		cfg := regexp.MustCompile(`(?m)^  ProxyCommand .*\n`).ReplaceAllString(string(files["config"]), "")
		files["config"] = []byte(strings.Replace(cfg, "  HostName srv.example\n", "  HostName "+host+"\n", 1))
		files["site.yml"] = []byte(play)
		d := t.TempDir()
		_ = os.Chmod(d, 0o755)
		for k, v := range files {
			mode := os.FileMode(0o444)
			if k == "id_ed25519" {
				mode = 0o400
			}
			if err := os.WriteFile(filepath.Join(d, k), v, mode); err != nil {
				t.Fatal(err)
			}
		}
		_ = os.Chown(filepath.Join(d, "id_ed25519"), 1000, 1000)
		args := []string{"run", "--rm", "--network", netw, "--user", "1000:1000", "--env", "HOME=/tmp",
			"--env", "ANSIBLE_SSH_ARGS=-F /ssh/config", "--env", "ANSIBLE_HOST_KEY_CHECKING=True", "--env", "ANSIBLE_LOCAL_TEMP=/tmp/a",
			"--env", "ANSIBLE_SSH_USETTY=False", "--env", "ANSIBLE_SSH_PIPELINING=True",
			"-v", d + ":/ssh:ro", "-v", d + "/passwd:/etc/passwd:ro", "-v", d + "/group:/etc/group:ro",
			"--entrypoint", "ansible-playbook", img, "-i", "/ssh/inventory.ini", "-e", "stage=" + stage, "/ssh/site.yml"}
		if check {
			args = append(args, "--check", "--diff")
		}
		out, err := exec.Command("docker", args...).CombinedOutput()
		return string(out), err
	}
	onServer := func(cmd string) string {
		c := exec.Command("sh", "-c", apply)
		c.Stdin = strings.NewReader(cmd + "\n")
		out, _ := c.CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	onServer("rm -f /etc/agentdev-applied /tmp/agentdev-marker-*")

	// merge stage: check principal as agentdev-check, --check: the become task cannot sudo, nothing in /etc changes
	out, err := run("check", "agentdev-web-check", "agentdev-check", true)
	if err == nil {
		t.Fatalf("check stage with become succeeded:\n%s", out)
	}
	if !strings.Contains(out, "sudo") && !strings.Contains(out, "password") {
		t.Logf("check output:\n%s", out)
	}
	if testing.Verbose() {
		t.Logf("check stage output:\n%s", out)
	}
	if got := onServer("cat /etc/agentdev-applied 2>/dev/null || echo none"); got != "none" {
		t.Fatalf("check stage changed /etc: %q", got)
	}
	// check_mode: false ran as the unprivileged check user only (no root file)
	if got := onServer("stat -c %U /tmp/agentdev-marker-check 2>/dev/null || echo none"); got != "agentdev-check" {
		t.Fatalf("marker owner %q", got)
	}
	// the check principal cannot log in as the apply user
	if out, err := run("check2", "agentdev-web-check", "agentdev", true); err == nil {
		t.Fatalf("check principal logged in as agentdev:\n%s", out)
	}
	// apply stage: apply principal as agentdev, with sudo
	if out, err := run("apply", "agentdev-web-apply", "agentdev", false); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if got := onServer("cat /etc/agentdev-applied"); got != "apply" {
		t.Fatalf("apply did not write /etc: %q", got)
	}
}
