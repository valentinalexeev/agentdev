package main

import (
	"context"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type kubeAgents struct {
	c  kubernetes.Interface
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
	return &kubeAgents{c: c, ns: ns}, nil
}

func (k *kubeAgents) List(ctx context.Context) ([]agent, error) {
	sts, err := k.c.AppsV1().StatefulSets(k.ns).List(ctx, metav1.ListOptions{LabelSelector: "app=coding-agent"})
	if err != nil {
		return nil, err
	}
	out := make([]agent, 0, len(sts.Items))
	for _, s := range sts.Items {
		out = append(out, toAgent(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// toAgent maps an agent StatefulSet (manifests/agent.yaml.tmpl) to its overview entry.
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
