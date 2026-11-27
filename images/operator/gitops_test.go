package main

import (
	"context"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// agentWrites records every write to an Agent's main resource (not status): with agents from Git (#58) Flux
// server-side apply owns the spec, so the operator may write spec.paused only, under its own field manager.
type agentWrite struct {
	verb, manager string
	patch         map[string]any
}

func recordingClient(t *testing.T, writes *[]agentWrite, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&v1.Agent{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
				if _, ok := o.(*v1.Agent); ok {
					*writes = append(*writes, agentWrite{verb: "update"})
				}
				return c.Update(ctx, o, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
				if _, ok := o.(*v1.Agent); ok {
					po := &client.PatchOptions{}
					po.ApplyOptions(opts)
					w := agentWrite{verb: "patch", manager: po.FieldManager}
					if b, err := p.Data(o); err == nil {
						_ = json.Unmarshal(b, &w.patch)
					}
					*writes = append(*writes, w)
				}
				return c.Patch(ctx, o, p, opts...)
			},
		}).Build()
}

// The security pause writes only spec.paused, with the field manager agentdev-operator.
func TestSecurityPauseWritesOnlyPaused(t *testing.T) {
	var writes []agentWrite
	c := recordingClient(t, &writes, agent("alice", func(a *v1.Agent) { a.Spec.MaxConcurrent = 3 }))
	col, _ := collector(t)
	col.Client, col.Direct = c, c
	p := pod("alice")
	spec, _ := parseLine(line(ts, `{"source":"forge","rule":"push-branch","severity":"high","action":"push to main"}`), p, "forge")
	if err := col.Record(context.Background(), p, spec); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 || writes[0].verb != "patch" || writes[0].manager != OperatorFieldManager {
		t.Fatalf("writes %+v, want one patch by %s", writes, OperatorFieldManager)
	}
	sp, _ := writes[0].patch["spec"].(map[string]any)
	if len(sp) != 1 || sp["paused"] != true {
		t.Errorf("patch spec %v, want only paused: true", writes[0].patch)
	}
	if a := getAgent(t, c, "alice"); !a.Spec.Paused || a.Spec.MaxConcurrent != 3 {
		t.Errorf("agent spec %+v", a.Spec)
	}
}

// The reconciler never writes an Agent's spec (status only), for every phase: Flux owns the spec in git mode.
func TestReconcilerNeverWritesAgentSpec(t *testing.T) {
	var writes []agentWrite
	c := recordingClient(t, &writes, agent("a1", nil), agent("a2", func(a *v1.Agent) { a.Spec.Paused = true }),
		agent("a3", func(a *v1.Agent) { a.Spec.ForgeSecret = "agent-other" }))
	r := &AgentReconciler{Client: c, D: Defaults{WorkerImage: "w", ForgeImage: "f"}}
	for _, n := range []string{"a1", "a2", "a3", "a1"} {
		reconcile(t, r, n)
	}
	if len(writes) != 0 {
		t.Errorf("reconciler wrote Agent objects: %+v", writes)
	}
}
