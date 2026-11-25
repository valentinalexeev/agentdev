package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const testWorker = "ghcr.io/example/agentdev-worker:latest"

func digestOf(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// toolchainPod: agent-<name>-0 whose toolchain init container ran image with imageID.
func toolchainPod(name, image, imageID string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-" + name + "-0", Namespace: ns,
		Labels: map[string]string{"app": "coding-agent", "agent": name}},
		Spec: corev1.PodSpec{InitContainers: []corev1.Container{{Name: toolchainContainer, Image: image}}},
		Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: toolchainContainer, Image: image,
			ImageID: imageID}}}}
}

func specDigest(t *testing.T, c client.Client, name string) string {
	t.Helper()
	for _, ct := range getSTS(t, c, name).Spec.Template.Spec.InitContainers {
		for _, e := range ct.Env {
			if e.Name == "TOOLCHAIN_SPEC" {
				var s toolchainSpec
				if err := json.Unmarshal([]byte(e.Value), &s); err != nil {
					t.Fatal(err)
				}
				return s.Digest
			}
		}
	}
	t.Fatal("no TOOLCHAIN_SPEC")
	return ""
}

func setPodImageID(t *testing.T, c client.Client, name, imageID string) {
	t.Helper()
	p := toolchainPod(name, testWorker, imageID)
	// the first reconcile may have replaced the pod (rollout, #174): the new pod of the moved tag reports the new digest
	_ = c.Delete(context.Background(), p)
	if err := c.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

// #538: the first digest is the base (no spec change); a re-pushed tag (new digest) changes the spec, so the pod rolls
// and installs under a new key; the same digest again keeps it stable.
func TestToolchainKeyFollowsImageDigest(t *testing.T) {
	r, c := setup(t, agent("alice", withGo), toolchainPod("alice", testWorker, "docker.io/x@"+digestOf('a')))
	r.D.WorkerImage = testWorker
	reconcile(t, r, "alice")
	if d := specDigest(t, c, "alice"); d != "" {
		t.Fatalf("first digest changed the spec: %q", d)
	}
	if got := getSTS(t, c, "alice").Annotations[toolchainBaseAnnotation]; got != testWorker+" "+digestOf('a') {
		t.Fatalf("base %q", got)
	}
	setPodImageID(t, c, "alice", "ghcr.io/example/agentdev-worker@"+digestOf('b'))
	reconcile(t, r, "alice")
	if d := specDigest(t, c, "alice"); d != digestOf('b') {
		t.Fatalf("re-pushed tag: spec digest %q, want %s", d, digestOf('b'))
	}
	reconcile(t, r, "alice")
	if d := specDigest(t, c, "alice"); d != digestOf('b') {
		t.Fatalf("not stable: %q", d)
	}
	// a new WORKER_IMAGE (upgrade): the old base no longer applies, the spec has the new image and no digest
	r.D.WorkerImage = "ghcr.io/example/agentdev-worker:v2"
	reconcile(t, r, "alice")
	if d := specDigest(t, c, "alice"); d != "" {
		t.Fatalf("upgrade kept the old digest: %q", d)
	}
}

// #538 fail-safe: no pod, an imageID without a digest, or a pod of another image change nothing.
func TestToolchainDigestFailSafe(t *testing.T) {
	for name, pod := range map[string]*corev1.Pod{
		"no pod":     nil,
		"no digest":  toolchainPod("alice", testWorker, "docker-pullable://worker"),
		"other tag":  toolchainPod("alice", "ghcr.io/example/agentdev-worker:old", "x@"+digestOf('c')),
		"no imageID": toolchainPod("alice", testWorker, ""),
	} {
		objs := []client.Object{agent("alice", withGo)}
		if pod != nil {
			objs = append(objs, pod)
		}
		r, c := setup(t, objs...)
		r.D.WorkerImage = testWorker
		reconcile(t, r, "alice")
		s := getSTS(t, c, "alice")
		if specDigest(t, c, "alice") != "" || s.Annotations[toolchainBaseAnnotation] != "" || s.Annotations[toolchainDigestAnnotation] != "" {
			t.Errorf("%s: annotations %v", name, s.Annotations)
		}
	}
}

// #538: without spec.toolchain the pod is not read and the StatefulSet gets no digest annotations.
func TestToolchainDigestInertWithoutToolchain(t *testing.T) {
	r, c := setup(t, agent("alice", nil), toolchainPod("alice", testWorker, "x@"+digestOf('a')))
	r.D.WorkerImage = testWorker
	reconcile(t, r, "alice")
	s := getSTS(t, c, "alice")
	if _, ok := s.Annotations[toolchainBaseAnnotation]; ok {
		t.Errorf("annotations %v", s.Annotations)
	}
	if _, ok := s.Annotations[toolchainDigestAnnotation]; ok {
		t.Errorf("annotations %v", s.Annotations)
	}
}
