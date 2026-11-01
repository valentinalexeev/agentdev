package main

// Approval requests (#67): the forge sidecar prints `AGENTDEV_APPROVAL_REQUEST {json}` when an agent asks for a
// permission (a Claude tool prompt, a push to a protected path). The collector (security.go) already follows every
// agent container's log; it hands each line to approvalFromLine, which accepts it only from the forge container
// (Claude runs in the agent container and can print any line there) and creates an Approval. The decision is written
// by agentdev-api (images/api/approvals.go); the operator only creates and prunes.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups=agentdev.dev,resources=approvals,verbs=get;list;watch;create;delete

const approvalPrefix = "AGENTDEV_APPROVAL_REQUEST "

var (
	approvalIDRe   = regexp.MustCompile(`^[a-f0-9]{16}$`)
	approvalHashRe = regexp.MustCompile(`^[a-f0-9]{64}$`)
	approvalJobRe  = regexp.MustCompile(`^(issue|pr)-[0-9]{1,10}$`)
	approvalRepoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

type rawApproval struct {
	ID          string `json:"id"`
	Repo        string `json:"repo"`
	Forge       string `json:"forge"`
	Job         string `json:"job"`
	Source      string `json:"source"`
	Tool        string `json:"tool"`
	Input       string `json:"input"`
	ContentHash string `json:"contentHash"`
	TimeoutSec  int    `json:"timeoutSeconds"`
}

// ApprovalHash is the content hash the sidecar computes (images/forge/approvals.go, same fields and order).
func ApprovalHash(id, agent, repo, job, source, tool, input string) string {
	h := sha256.New()
	for _, s := range []string{id, agent, repo, job, source, tool, input} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// approvalFromLine parses one timestamped log line of container into an Approval. ok=false for any other line, a line
// from another container, or a line that does not validate (including a content hash that does not match: the sidecar
// and the object must agree, or no decision could ever apply).
func approvalFromLine(line string, pod *corev1.Pod, container, ns string) (*v1.Approval, bool) {
	if container != forgeContainer {
		return nil, false
	}
	ts, rest, found := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
	if !found || !strings.HasPrefix(rest, approvalPrefix) {
		return nil, false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return nil, false
	}
	var r rawApproval
	if err := json.Unmarshal([]byte(strings.TrimPrefix(rest, approvalPrefix)), &r); err != nil {
		return nil, false
	}
	agent := agentOf(pod)
	if r.Forge == "" {
		r.Forge = "gitea"
	}
	if !approvalIDRe.MatchString(r.ID) || !approvalHashRe.MatchString(r.ContentHash) || !approvalJobRe.MatchString(r.Job) ||
		!approvalRepoRe.MatchString(r.Repo) || len(r.Repo) > 201 || (r.Source != "claude" && r.Source != "push") ||
		(r.Forge != "gitea" && r.Forge != "github") || r.Tool == "" || len(r.Tool) > 128 || len(r.Input) > 4100 {
		return nil, false
	}
	if ApprovalHash(r.ID, agent, r.Repo, r.Job, r.Source, r.Tool, r.Input) != r.ContentHash {
		return nil, false
	}
	timeout := time.Duration(r.TimeoutSec) * time.Second
	if timeout <= 0 || timeout > 24*time.Hour {
		timeout = 30 * time.Minute
	}
	return &v1.Approval{
		ObjectMeta: metav1.ObjectMeta{Name: agent + "-" + r.ID, Namespace: ns,
			Labels: map[string]string{"agent": agent, "agentdev/job": r.Job, "agentdev/source": r.Source}},
		Spec: v1.ApprovalSpec{ID: r.ID, Agent: agent, Repo: r.Repo, Forge: r.Forge, Job: r.Job, Source: r.Source,
			Tool: r.Tool, Input: r.Input, ContentHash: r.ContentHash, Created: metav1.NewTime(t),
			Expires: metav1.NewTime(t.Add(timeout))},
	}, true
}

// recordApproval creates the Approval for an approval line (idempotent by name). handled=false: not an approval line.
func (c *Collector) recordApproval(ctx context.Context, line string, p *corev1.Pod, container string) (handled bool, err error) {
	a, ok := approvalFromLine(line, p, container, c.NS)
	if !ok {
		return false, nil
	}
	if err := c.Create(ctx, a); err != nil && !apierrors.IsAlreadyExists(err) {
		return true, err
	}
	log.FromContext(ctx).Info("approval request", "agent", a.Spec.Agent, "id", a.Spec.ID, "job", a.Spec.Job, "tool", a.Spec.Tool)
	return true, nil
}

// pruneApprovals deletes Approvals created more than ApprovalRetention ago (default 90 days, #67). The audit log keeps
// the decisions.
func (c *Collector) pruneApprovals(ctx context.Context) error {
	keep := c.C.ApprovalRetention
	if keep <= 0 {
		keep = 90 * 24 * time.Hour
	}
	var list v1.ApprovalList
	if err := c.List(ctx, &list, client.InNamespace(c.NS)); err != nil {
		return err
	}
	cut := c.Now().Add(-keep)
	for i := range list.Items {
		if a := &list.Items[i]; a.Spec.Created.Time.Before(cut) {
			if err := c.Delete(ctx, a); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return nil
}
