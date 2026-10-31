package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

var securityEventsGVR = schema.GroupVersionResource{Group: "agentdev.dev", Version: "v1alpha1", Resource: "securityevents"}

// fromSecurityEventObject maps a SecurityEvent object to the schema record; ok=false if it lacks the required fields.
func fromSecurityEventObject(u *unstructured.Unstructured) (securityEvent, bool) {
	str := func(f string) string { v, _, _ := unstructured.NestedString(u.Object, "spec", f); return v }
	e := securityEvent{Schema: str("schema"), Agent: str("agent"), Job: str("job"), Source: str("source"), Rule: str("rule"),
		Severity: str("severity"), Action: str("action"), Pod: str("pod"), Container: str("container"), Hash: str("hash")}
	t, err := time.Parse(time.RFC3339, str("time"))
	if err != nil || e.Hash == "" || e.Agent == "" || e.Rule == "" {
		return e, false
	}
	e.Time = t.UTC()
	if e.Schema == "" {
		e.Schema = schemaV1
	}
	return e, true
}

// sink stores one event and, if it is new, sends it to the outputs.
type sink struct {
	store eventStore
	out   *webhook // nil: no webhook configured
}

func (s *sink) add(ctx context.Context, e securityEvent) {
	isNew, err := s.store.Insert(ctx, &e)
	if err != nil {
		log.Printf("store security event %s: %v", e.Hash, err)
		return
	}
	if isNew && s.out != nil {
		s.out.Enqueue(e)
	}
}

// watchSecurityEvents follows SecurityEvent objects in ns. An informer lists everything on start and after each
// reconnect; the hash makes re-adding an event a no-op, so nothing is stored or sent twice.
func watchSecurityEvents(ctx context.Context, d dynamic.Interface, ns string, s *sink) {
	f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(d, 10*time.Minute, ns, nil)
	inf := f.ForResource(securityEventsGVR).Informer()
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: func(obj any) {
		if u, ok := obj.(*unstructured.Unstructured); ok {
			if e, ok := fromSecurityEventObject(u); ok {
				s.add(ctx, e)
			}
		}
	}})
	f.Start(ctx.Done())
}

// webhook posts each new event as JSON to URL, signed with HMAC-SHA256 over "<timestamp>.<body>" in
// X-Agentdev-Signature: sha256=<hex> (timestamp in X-Agentdev-Timestamp, Unix seconds) so the receiver can check
// origin and reject replays. Failed deliveries are retried with exponential backoff, up to Attempts.
type webhook struct {
	URL      string
	Secret   []byte
	Client   *http.Client
	Attempts int
	Backoff  time.Duration // first retry delay; doubles each time, at most 5 minutes
	Now      func() time.Time
	Sleep    func(context.Context, time.Duration) bool
	queue    chan securityEvent
}

func newWebhook(url string, secret []byte) *webhook {
	return &webhook{URL: url, Secret: secret, Client: &http.Client{Timeout: 10 * time.Second}, Attempts: 8,
		Backoff: 2 * time.Second, Now: time.Now, Sleep: sleepCtx, queue: make(chan securityEvent, 1000)}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Enqueue never blocks the watch: when the queue is full the event is dropped from the webhook (it is still in the
// table and in the cluster log) and the drop is logged.
func (w *webhook) Enqueue(e securityEvent) {
	select {
	case w.queue <- e:
	default:
		log.Printf("webhook queue full, event %d not sent (still stored)", e.ID)
	}
}

func (w *webhook) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-w.queue:
			if err := w.deliver(ctx, e); err != nil {
				log.Printf("webhook: event %d not delivered: %v", e.ID, err)
			}
		}
	}
}

func sign(secret []byte, ts string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(ts))
	m.Write([]byte("."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func (w *webhook) deliver(ctx context.Context, e securityEvent) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	delay := w.Backoff
	var last error
	for i := 0; i < w.Attempts; i++ {
		if i > 0 {
			if !w.Sleep(ctx, delay) {
				return ctx.Err()
			}
			if delay *= 2; delay > 5*time.Minute {
				delay = 5 * time.Minute
			}
		}
		if last = w.post(ctx, body); last == nil {
			return nil
		}
	}
	return fmt.Errorf("after %d attempts: %w", w.Attempts, last)
}

func (w *webhook) post(ctx context.Context, body []byte) error {
	ts := strconv.FormatInt(w.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "agentdev-api")
	req.Header.Set("X-Agentdev-Schema", schemaV1)
	req.Header.Set("X-Agentdev-Timestamp", ts)
	req.Header.Set("X-Agentdev-Signature", sign(w.Secret, ts, body))
	resp, err := w.Client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
