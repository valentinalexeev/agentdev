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
	"sync"
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
	e.Untrusted, _, _ = unstructured.NestedBool(u.Object, "spec", "untrusted")
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

// sink stores one event and, if it is new, sends it to the outputs (webhook, syslog, OTLP: outputs.go).
type sink struct {
	store eventStore
	outs  []*webhook // the configured outputs, each with its own fair queue
}

func (s *sink) add(ctx context.Context, e securityEvent) {
	isNew, err := s.store.Insert(ctx, &e)
	if err != nil {
		log.Printf("store security event %s: %v", e.Hash, err)
		return
	}
	if isNew {
		for _, o := range s.outs {
			o.Enqueue(e)
		}
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

// webhook is the queue and retry engine of every output. By default it posts each new event as JSON to URL, signed
// with HMAC-SHA256 over "<timestamp>.<body>" in X-Agentdev-Signature: sha256=<hex> (timestamp in
// X-Agentdev-Timestamp, Unix seconds) so the receiver can check origin and reject replays. Failed deliveries are
// retried with exponential backoff, up to Attempts.
//
// Fair queueing: each agent has its own bounded queue (PerAgent events); the workers take one event per agent in
// turn. A flooding agent fills and overflows only its own queue, so it cannot push other agents' events out. Drops
// are counted per agent (Dropped) and logged; the event stays in the table and the cluster log. Delivery is at most
// once after the queue: an event that fails Attempts times, or is queued when the API stops, is not sent again.
type webhook struct {
	URL      string
	Secret   []byte
	Client   *http.Client
	Attempts int
	Backoff  time.Duration // first retry delay; doubles each time, at most MaxBackoff
	// MaxBackoff caps the retry delay; 0 means 5 minutes
	MaxBackoff time.Duration
	Now        func() time.Time
	Sleep      func(context.Context, time.Duration) bool
	PerAgent   int // queue size per agent
	Workers    int // parallel deliveries
	// Name is used in log lines. Encode and Send replace the webhook's JSON body and signed POST, so syslog and OTLP
	// (outputs.go) reuse the same queues and retries. nil: the webhook behaviour.
	Name   string
	Encode func(securityEvent) ([]byte, error)
	Send   func(context.Context, []byte) error

	mu      sync.Mutex
	queues  map[string][]securityEvent
	order   []string // agents with queued events, round robin
	dropped map[string]int64
	ready   chan struct{}
}

func newWebhook(url string, secret []byte) *webhook {
	return &webhook{URL: url, Secret: secret, Client: &http.Client{Timeout: 10 * time.Second}, Attempts: 8,
		Backoff: 2 * time.Second, Now: time.Now, Sleep: sleepCtx, PerAgent: 200, Workers: 4}
}

func (w *webhook) init() {
	if w.queues == nil {
		w.queues, w.dropped, w.ready = map[string][]securityEvent{}, map[string]int64{}, make(chan struct{}, 1)
	}
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

// Enqueue never blocks the watch: when the agent's queue is full the event is dropped from the webhook (it is still
// in the table and in the cluster log), counted and logged.
func (w *webhook) Enqueue(e securityEvent) {
	w.mu.Lock()
	w.init()
	q := w.queues[e.Agent]
	if len(q) >= w.PerAgent {
		w.dropped[e.Agent]++
		n := w.dropped[e.Agent]
		w.mu.Unlock()
		log.Printf("%s queue of agent %s full, event %d not sent (still stored; %d dropped for this agent)", w.name(), e.Agent, e.ID, n)
		return
	}
	if len(q) == 0 {
		w.order = append(w.order, e.Agent)
	}
	w.queues[e.Agent] = append(q, e)
	w.mu.Unlock()
	select {
	case w.ready <- struct{}{}:
	default:
	}
}

// next takes the head of the next agent's queue in round robin; ok=false when all queues are empty.
func (w *webhook) next() (securityEvent, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.init()
	if len(w.order) == 0 {
		return securityEvent{}, false
	}
	a := w.order[0]
	w.order = w.order[1:]
	q := w.queues[a]
	e := q[0]
	if len(q) == 1 {
		delete(w.queues, a)
	} else {
		w.queues[a] = q[1:]
		w.order = append(w.order, a)
	}
	return e, true
}

// Dropped is the number of events dropped from the webhook per agent since start. It copies the map under the lock,
// so callers (tests today, a future metrics endpoint) get a stable snapshot without racing the delivery goroutine.
func (w *webhook) Dropped() map[string]int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]int64{}
	for k, v := range w.dropped {
		out[k] = v
	}
	return out
}

func (w *webhook) Run(ctx context.Context) {
	w.mu.Lock()
	w.init()
	w.mu.Unlock()
	n := w.Workers
	if n < 1 {
		n = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if e, ok := w.next(); ok {
					if err := w.deliver(ctx, e); err != nil {
						log.Printf("%s: event %d not delivered: %v", w.name(), e.ID, err)
					}
					continue
				}
				select {
				case <-ctx.Done():
					return
				case <-w.ready:
					select { // pass the wake-up on: other workers may find work too
					case w.ready <- struct{}{}:
					default:
					}
				}
			}
		}()
	}
	wg.Wait()
}

func sign(secret []byte, ts string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(ts))
	m.Write([]byte("."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func (w *webhook) name() string {
	if w.Name == "" {
		return "webhook"
	}
	return w.Name
}

func (w *webhook) deliver(ctx context.Context, e securityEvent) error {
	encode, send := w.Encode, w.Send
	if encode == nil {
		encode = func(e securityEvent) ([]byte, error) { return json.Marshal(e) }
	}
	if send == nil {
		send = w.post
	}
	body, err := encode(e)
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
			maxB := w.MaxBackoff
			if maxB == 0 {
				maxB = 5 * time.Minute
			}
			if delay *= 2; delay > maxB {
				delay = maxB
			}
		}
		if last = send(ctx, body); last == nil {
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
