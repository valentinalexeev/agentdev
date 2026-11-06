package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// cacheOptions scopes the manager cache per kind (#161). Everything the agent controller watches (Agent, StatefulSet,
// Service, ...) is cached in the agents namespace only. Executor kinds are cached in EXEC_NS only, and Pods (agents'
// and executor Jobs') in both. The Roles in manifests/operator.yaml.tmpl grant list/watch exactly there; a kind cached
// in a namespace without list/watch never syncs and the operator crash-loops. Secrets are read by name through the
// API reader (no list/watch in EXEC_NS) and are therefore not cached there.
func cacheOptions(ns, execNS string) cache.Options {
	o := cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}}
	if execNS == "" {
		return o
	}
	exec := cache.Config{}
	only := func() map[string]cache.Config { return map[string]cache.Config{execNS: exec} }
	o.ByObject = map[client.Object]cache.ByObject{
		&v1.Executor{}:                  {Namespaces: only()},
		&v1.ExecutorRun{}:               {Namespaces: only()},
		&batchv1.Job{}:                  {Namespaces: only()},
		&corev1.PersistentVolumeClaim{}: {Namespaces: only()},
		&corev1.Pod{}:                   {Namespaces: map[string]cache.Config{ns: {}, execNS: exec}},
	}
	return o
}

// cacheSynced is a readiness check that passes once every informer of c has synced.
func cacheSynced(c cache.Cache) healthz.Checker {
	return func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), time.Second)
		defer cancel()
		if !c.WaitForCacheSync(ctx) {
			return errors.New("caches not synced")
		}
		return nil
	}
}
