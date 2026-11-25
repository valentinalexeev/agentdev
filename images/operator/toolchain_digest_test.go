package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const testWorker = "ghcr.io/example/agentdev-worker:latest"

func digestOf(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// toolchainPod: agent-<name>-0 whose state-init (the tag probe) ran image with imageID.
func toolchainPod(name, image, imageID string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-" + name + "-0", Namespace: ns,
		Labels: map[string]string{"app": "coding-agent", "agent": name}},
		Spec: corev1.PodSpec{InitContainers: []corev1.Container{{Name: "state-init", Image: image}}},
		Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: "state-init", Image: image,
			ImageID: imageID}}}}
}

// images: the images of the toolchain init container and the agent container
func images(t *testing.T, c client.Client, name string) (tc, ag string) {
	t.Helper()
	p := getSTS(t, c, name).Spec.Template.Spec
	for _, ct := range p.InitContainers {
		if ct.Name == toolchainContainer {
			tc = ct.Image
		}
	}
	for _, ct := range p.Containers {
		if ct.Name == "agent" {
			ag = ct.Image
		}
	}
	return tc, ag
}

const testRepo = "ghcr.io/example/agentdev-worker"

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
	r, c := setup(t, agent("alice", withGo), toolchainPod("alice", testWorker, testRepo+"@"+digestOf('a')))
	r.D.WorkerImage, r.D.PullPolicy = testWorker, "IfNotPresent"
	reconcile(t, r, "alice")
	if d := specDigest(t, c, "alice"); d != "" {
		t.Fatalf("first digest changed the spec: %q", d)
	}
	if got := getSTS(t, c, "alice").Annotations[toolchainBaseAnnotation]; got != testWorker+" "+digestOf('a') {
		t.Fatalf("base %q", got)
	}
	// the toolchain and agent containers run the digest the key is for
	if tc, ag := images(t, c, "alice"); tc != testRepo+"@"+digestOf('a') || ag != tc {
		t.Fatalf("not pinned: toolchain %s, agent %s", tc, ag)
	}
	setPodImageID(t, c, "alice", testRepo+"@"+digestOf('b'))
	reconcile(t, r, "alice")
	if d := specDigest(t, c, "alice"); d != digestOf('b') {
		t.Fatalf("re-pushed tag: spec digest %q, want %s", d, digestOf('b'))
	}
	if tc, ag := images(t, c, "alice"); tc != testRepo+"@"+digestOf('b') || ag != tc {
		t.Fatalf("not pinned to b: toolchain %s, agent %s", tc, ag)
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
	if tc, ag := images(t, c, "alice"); tc != r.D.WorkerImage || ag != tc {
		t.Fatalf("upgrade kept the old pin: toolchain %s, agent %s", tc, ag)
	}
}

// #551 review: nodes with different cached images report a, b, a. The digest only moves forward: the key and the pin
// stay at b, so the pod does not roll back and forth.
func TestToolchainDigestNoFlip(t *testing.T) {
	r, c := setup(t, agent("alice", withGo), toolchainPod("alice", testWorker, testRepo+"@"+digestOf('a')))
	r.D.WorkerImage, r.D.PullPolicy = testWorker, "IfNotPresent"
	reconcile(t, r, "alice")
	setPodImageID(t, c, "alice", testRepo+"@"+digestOf('b'))
	reconcile(t, r, "alice")
	before := getSTS(t, c, "alice").Spec.Template
	setPodImageID(t, c, "alice", testRepo+"@"+digestOf('a')) // a node that still has the old image
	reconcile(t, r, "alice")
	if d := specDigest(t, c, "alice"); d != digestOf('b') {
		t.Fatalf("flipped back: spec digest %q", d)
	}
	if after := getSTS(t, c, "alice").Spec.Template; !equality.Semantic.DeepEqual(before, after) {
		t.Fatal("the pod template changed on a stale digest")
	}
	// a pod running the pinned image is not evidence of the tag: it never moves the key
	setPodImageID(t, c, "alice", testRepo+"@"+digestOf('c'))
	p := toolchainPod("alice", testRepo+"@"+digestOf('b'), testRepo+"@"+digestOf('c'))
	_ = c.Delete(context.Background(), p)
	if err := c.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "alice")
	if d := specDigest(t, c, "alice"); d != digestOf('b') {
		t.Fatalf("a pinned pod moved the key: %q", d)
	}
}

// #538: pull policy Never (kind): the key follows the digest, the containers keep the tag (a loaded image has no
// registry); a WORKER_IMAGE already pinned to a digest is left alone.
func TestToolchainDigestPullNeverAndPinnedImage(t *testing.T) {
	r, c := setup(t, agent("alice", withGo), toolchainPod("alice", testWorker, testRepo+"@"+digestOf('a')))
	r.D.WorkerImage, r.D.PullPolicy = testWorker, "Never"
	reconcile(t, r, "alice")
	if tc, ag := images(t, c, "alice"); tc != testWorker || ag != testWorker {
		t.Fatalf("Never: pinned %s %s", tc, ag)
	}
	pinned := testRepo + "@" + digestOf('a')
	r2, c2 := setup(t, agent("bob", withGo), toolchainPod("bob", pinned, pinned))
	r2.D.WorkerImage = pinned
	reconcile(t, r2, "bob")
	if s := getSTS(t, c2, "bob"); s.Annotations[toolchainBaseAnnotation] != "" || specDigest(t, c2, "bob") != "" {
		t.Fatalf("digest image: %v", s.Annotations)
	}
}

// #538 fail-safe: no pod, an imageID without a digest, or a pod of another image change nothing.
func TestToolchainDigestFailSafe(t *testing.T) {
	for name, pod := range map[string]*corev1.Pod{
		"no pod":     nil,
		"no digest":  toolchainPod("alice", testWorker, "docker-pullable://worker"),
		"image id":   toolchainPod("alice", testWorker, digestOf('d')),
		"other tag":  toolchainPod("alice", "ghcr.io/example/agentdev-worker:old", "x@"+digestOf('c')),
		"no imageID": toolchainPod("alice", testWorker, ""),
	} {
		objs := []client.Object{agent("alice", withGo)}
		if pod != nil {
			objs = append(objs, pod)
		}
		r, c := setup(t, objs...)
		r.D.WorkerImage, r.D.PullPolicy = testWorker, "IfNotPresent"
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
	r.D.WorkerImage, r.D.PullPolicy = testWorker, "IfNotPresent"
	reconcile(t, r, "alice")
	s := getSTS(t, c, "alice")
	if _, ok := s.Annotations[toolchainBaseAnnotation]; ok {
		t.Errorf("annotations %v", s.Annotations)
	}
	if _, ok := s.Annotations[toolchainDigestAnnotation]; ok {
		t.Errorf("annotations %v", s.Annotations)
	}
}
