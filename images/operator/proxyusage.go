package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// Usage and events from the Claude credential proxy (#66). With the proxy on (Collector.ProxyUsage), the proxy is
// the only usage source: it reads the rate-limit headers and token counts of every model response itself, outside
// the agent pod, so the model cannot forge a report (closes R47 of #93). Usage lines from agent pods, including the
// forge sidecar's POST /usage channel, are then ignored and reported as usage-line-forged.

const proxyContainer = "proxy"

var agentNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// usageFromAgentPod: whether a usage line from this agent pod container is taken.
func (c *Collector) usageFromAgentPod(container string) bool {
	return !c.ProxyUsage && container == forgeContainer
}

// scanProxy follows the log of every running claude-proxy pod.
func (c *Collector) scanProxy(ctx context.Context) {
	if !c.ProxyUsage {
		return
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(c.NS), client.MatchingLabels{"app": "claude-proxy"}); err != nil {
		log.FromContext(ctx).Error(err, "list claude-proxy pods")
		return
	}
	for i := range pods.Items {
		p := pods.Items[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		key := string(p.UID) + "/" + proxyContainer
		c.mu.Lock()
		busy := c.streams[key]
		c.streams[key] = true
		c.mu.Unlock()
		if !busy {
			go c.followProxy(ctx, &p, key)
		}
	}
}

func (c *Collector) followProxy(ctx context.Context, p *corev1.Pod, key string) {
	defer func() { c.mu.Lock(); delete(c.streams, key); c.mu.Unlock() }()
	l := log.FromContext(ctx).WithValues("pod", p.Name, "container", proxyContainer)
	since, err := c.position(ctx, p, proxyContainer)
	if err != nil {
		l.Error(err, "read position")
		return
	}
	rc, err := c.Logs(ctx, c.NS, p.Name, proxyContainer, since)
	if err != nil {
		l.V(1).Info("open log", "err", err.Error())
		return
	}
	defer rc.Close()
	if err := c.consumeProxy(ctx, rc, p); err != nil && ctx.Err() == nil {
		l.Error(err, "read log")
	}
}

// consumeProxy takes usage lines and security events from a claude-proxy log. The agent of an event comes from the
// line: the proxy names it from the verified key, and the model cannot write this log.
func (c *Collector) consumeProxy(ctx context.Context, r io.Reader, p *corev1.Pod) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if isUsageLine(line) {
			if c.Usage != nil {
				if u, at, ok := parseUsage(line); ok {
					if err := c.Usage.ObserveProxy(ctx, p, u, at); err != nil {
						log.FromContext(ctx).Error(err, "record proxy usage")
					}
				}
			}
			continue
		}
		spec, ok := parseProxyLine(line, p)
		if !ok {
			continue
		}
		if ok, first := c.admit(string(p.UID)+"/"+proxyContainer, spec.Hash, spec.Time.Time); !ok {
			c.Dropped.Add(1)
			if !first {
				continue
			}
			spec = rateLimited(spec, c.C.RatePerMin)
		}
		if err := c.Record(ctx, p, spec); err != nil {
			return fmt.Errorf("record proxy security event %s: %w", spec.Rule, err)
		}
		if err := c.savePosition(ctx, p, proxyContainer, spec.Time.Time); err != nil {
			log.FromContext(ctx).Error(err, "save log position")
		}
	}
	return sc.Err()
}

func parseProxyLine(line string, p *corev1.Pod) (v1.SecurityEventSpec, bool) {
	ts, rest, found := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
	if !found || !strings.HasPrefix(rest, eventPrefix) {
		return v1.SecurityEventSpec{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return v1.SecurityEventSpec{}, false
	}
	var e rawEvent
	if err := json.Unmarshal([]byte(strings.TrimPrefix(rest, eventPrefix)), &e); err != nil || e.Rule == "" {
		return v1.SecurityEventSpec{}, false
	}
	switch e.Severity {
	case "low", "medium", "high":
	default:
		e.Severity = "high"
	}
	if !agentNameRE.MatchString(e.Agent) {
		e.Agent = "claude-proxy" // a refused request without a valid key names no agent
	}
	h := sha256.Sum256([]byte(string(p.UID) + "\x00" + proxyContainer + "\x00" + ts + "\x00" + rest))
	return v1.SecurityEventSpec{Schema: v1.SchemaV1, Source: "claude-proxy", Rule: clip(e.Rule, 80),
		Severity: e.Severity, Agent: e.Agent, Action: clip(e.Action, 503),
		Time: metav1.NewTime(t), Pod: p.Name, Container: proxyContainer, Hash: hex.EncodeToString(h[:])}, true
}
