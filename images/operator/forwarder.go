package main

// External forwarder watch (#213 part 2): with ingress.mode external (#160) a forwarder outside agentdev publishes the
// public hosts. When it stays not Ready (for example an expired auth key), the public hosts answer 404 and nothing in
// agentdev shows it. This runnable lists the forwarder pods every Every and, when no pod has been Ready for longer than
// After, logs a warning and records one SecurityEvent (source ingress, rule forwarder-not-ready) per outage, so the
// event reaches the SIEM outputs of the API (#70). It never pauses an agent. Off unless FORWARDER_NS and
// FORWARDER_SELECTOR are set (deploy.yml sets them from ingress.forwarder namespace and pod_selector).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// The pods are read in the forwarder namespace with the API reader (not the cache, which holds the agents namespace
// only): manifests/operator.yaml.tmpl adds a Role with pods get/list there when the watch is on.

// forwarderAgent is the agent field of forwarder events: the schema needs one, and no agent is the cause.
const forwarderAgent = "ingress-forwarder"

type ForwarderWatch struct {
	Reader   client.Reader // reads pods in NS (API reader)
	Writer   client.Client // writes the SecurityEvent in EventsNS
	NS       string        // forwarder namespace
	Selector map[string]string
	EventsNS string        // agents namespace, where SecurityEvents live
	After    time.Duration // not Ready for longer than this: warning and event (default 10 minutes)
	Every    time.Duration // check interval (default 30 s)
	Now      func() time.Time

	since    time.Time // start of the current outage; zero while Ready
	reported bool      // the event for the current outage is recorded
}

// parseSelector reads "k=v,k2=v2" (FORWARDER_SELECTOR). Empty or a malformed pair: nil.
func parseSelector(s string) map[string]string {
	m := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		if kv = strings.TrimSpace(kv); kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// forwarderPodReady: Running, not deleting, and Ready. Not named podReady: #260 (drain.go) has a podReady too.
func forwarderPodReady(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (w *ForwarderWatch) selectorText() string {
	keys := make([]string, 0, len(w.Selector))
	for k := range w.Selector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + w.Selector[k]
	}
	return strings.Join(parts, ",")
}

func (w *ForwarderWatch) Start(ctx context.Context) error {
	every := w.Every
	if every <= 0 {
		every = 30 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := w.Check(ctx); err != nil {
			log.FromContext(ctx).Error(err, "forwarder watch", "namespace", w.NS, "selector", w.selectorText())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// Check runs one round: list the pods, update the outage state, record the event once the outage is long enough.
func (w *ForwarderWatch) Check(ctx context.Context) error {
	var pods corev1.PodList
	if err := w.Reader.List(ctx, &pods, client.InNamespace(w.NS),
		client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(w.Selector)}); err != nil {
		return err // API error: keep the state, try again on the next tick
	}
	now := w.Now()
	total, ready, first := len(pods.Items), 0, ""
	for i := range pods.Items {
		if forwarderPodReady(&pods.Items[i]) {
			ready++
		}
	}
	if total > 0 {
		names := make([]string, 0, total)
		for _, p := range pods.Items {
			names = append(names, p.Name)
		}
		sort.Strings(names)
		first = names[0]
	}
	lg := log.FromContext(ctx)
	if ready > 0 {
		if w.reported {
			lg.Info("external forwarder is Ready again", "namespace", w.NS, "selector", w.selectorText(),
				"down", now.Sub(w.since).Round(time.Second).String())
		}
		w.since, w.reported = time.Time{}, false
		return nil
	}
	if w.since.IsZero() {
		w.since = now
	}
	after := w.After
	if after <= 0 {
		after = 10 * time.Minute
	}
	if w.reported || now.Sub(w.since) < after {
		return nil
	}
	action := fmt.Sprintf("external forwarder not Ready for %s: %d pods, 0 Ready, namespace %s, selector %s; the public hosts do not answer",
		now.Sub(w.since).Round(time.Minute), total, w.NS, w.selectorText())
	lg.Info("WARNING "+action, "namespace", w.NS, "selector", w.selectorText(), "pods", total)
	h := sha256.Sum256([]byte("forwarder-not-ready\x00" + w.NS + "\x00" + w.selectorText() + "\x00" + w.since.UTC().Format(time.RFC3339Nano)))
	spec := v1.SecurityEventSpec{Schema: v1.SchemaV1, Source: "ingress", Rule: "forwarder-not-ready", Severity: "medium",
		Agent: forwarderAgent, Action: clip(action, 500), Time: metav1.NewTime(now), Pod: first, Container: "",
		Hash: hex.EncodeToString(h[:])}
	ev := &v1.SecurityEvent{ObjectMeta: metav1.ObjectMeta{Name: eventName(spec), Namespace: w.EventsNS,
		Labels: map[string]string{"agent": spec.Agent, "agentdev/severity": spec.Severity, "agentdev/job": "",
			"agentdev/source": spec.Source}}, Spec: spec}
	if err := w.Writer.Create(ctx, ev); err != nil && !apierrors.IsAlreadyExists(err) {
		return err // not reported: the next tick tries again
	}
	w.reported = true
	return nil
}
