package main

import (
	"context"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// agentsGVR is the Agent resource of the operator (#47). The API reads it through the dynamic client, so it does
// not depend on the operator's Go module.
var agentsGVR = schema.GroupVersionResource{Group: "agentdev.dev", Version: "v1alpha1", Resource: "agents"}

type kubeAgents struct {
	c  kubernetes.Interface
	d  dynamic.Interface
	ns string
}

// newKubeAgents uses the in-cluster service account, or KUBECONFIG when run outside the cluster.
func newKubeAgents(ns string) (*kubeAgents, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, err
		}
	}
	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	d, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &kubeAgents{c: c, d: d, ns: ns}, nil
}

// List returns every agent: the Agent objects (#57) with their spec and status, plus any agent StatefulSet that has
// no Agent yet (deployed before the operator; shown read-only, `managed: false`).
func (k *kubeAgents) List(ctx context.Context) ([]agent, error) {
	byName := map[string]agent{}
	if k.d != nil {
		if ags, err := k.d.Resource(agentsGVR).Namespace(k.ns).List(ctx, metav1.ListOptions{}); err == nil {
			for _, u := range ags.Items {
				a := fromAgentObject(u)
				byName[a.Name] = a
			}
		} // a cluster without the CRD (operator not installed) still lists its StatefulSets
	}
	sts, err := k.c.AppsV1().StatefulSets(k.ns).List(ctx, metav1.ListOptions{LabelSelector: "app=coding-agent"})
	if err != nil {
		return nil, err
	}
	for _, s := range sts.Items {
		a := toAgent(s)
		if m, ok := byName[a.Name]; ok {
			m.Replicas, m.Ready = a.Replicas, a.Ready // live pod counts from the StatefulSet
			if m.Phase == "" {
				m.Phase = a.Phase
			}
			byName[a.Name] = m
			continue
		}
		byName[a.Name] = a
	}
	out := make([]agent, 0, len(byName))
	for _, a := range byName {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// fromAgentObject maps an Agent (spec + status written by the operator) to its overview entry.
func fromAgentObject(u unstructured.Unstructured) agent {
	str := func(f ...string) string { v, _, _ := unstructured.NestedString(u.Object, f...); return v }
	num := func(f ...string) int64 { v, _, _ := unstructured.NestedInt64(u.Object, f...); return v }
	flag := func(f ...string) bool { v, _, _ := unstructured.NestedBool(u.Object, f...); return v }
	a := agent{Name: u.GetName(), Managed: true, Forge: str("spec", "forge"), Phase: str("status", "phase"),
		MaxConcurrent: int32(num("spec", "maxConcurrent")), Paused: flag("spec", "paused"),
		ScaleToZero: flag("spec", "scaleToZero"), Ready: int32(num("status", "ready"))}
	if r := str("spec", "repo"); r != "" {
		a.Repos = []string{r}
	}
	if a.Forge == "" {
		a.Forge = "gitea"
	}
	return a
}

// toAgent maps an agent StatefulSet (rendered by the operator, images/operator) to its overview entry.
func toAgent(s appsv1.StatefulSet) agent {
	a := agent{Name: s.Labels["agent"], Forge: s.Annotations["agentdev/forge"], Ready: s.Status.ReadyReplicas}
	if a.Name == "" {
		a.Name = strings.TrimPrefix(s.Name, "agent-")
	}
	if s.Spec.Replicas != nil {
		a.Replicas = *s.Spec.Replicas
	}
	for _, r := range strings.Split(s.Annotations["agentdev/repos"], ",") {
		if r = strings.TrimSpace(r); r != "" {
			a.Repos = append(a.Repos, r)
		}
	}
	switch {
	case s.Annotations["agentdev/blocked"] == "true":
		a.Phase = "blocked"
	case a.Replicas == 0:
		a.Phase = "stopped" // scaled to zero; the router wakes it on an event
	case a.Ready >= a.Replicas:
		a.Phase = "running"
	default:
		a.Phase = "starting"
	}
	return a
}
