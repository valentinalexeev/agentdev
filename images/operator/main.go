// agentdev operator (#47): reconciles Agent resources (agentdev.dev/v1alpha1) into agent StatefulSets and Services.
// Day-2 lifecycle (add, remove, pause) moves here from scripts; cluster bootstrap stays outside (#4).
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	var leader bool
	flag.BoolVar(&leader, "leader-elect", false, "leader election (for more than one replica)")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)

	ns := env("AGENTS_NS", "coding-agents")
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}}, // agents namespace only
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: ":8081",
		LeaderElection:         leader,
		LeaderElectionID:       "agentdev-operator.agentdev.dev",
	})
	if err != nil {
		log.Error(err, "manager")
		os.Exit(1)
	}
	r := &AgentReconciler{Client: mgr.GetClient(), D: Defaults{
		WorkerImage:    env("WORKER_IMAGE", "ghcr.io/valentinalexeev/agentdev-worker:latest"),
		PullPolicy:     env("IMAGE_PULL_POLICY", "IfNotPresent"),
		ForgeURL:       os.Getenv("FORGE_URL"),
		ForgePublicURL: os.Getenv("FORGE_PUBLIC_URL"),
		Reviewer:       os.Getenv("REVIEWER"),
		BotboardURL:    os.Getenv("BOTBOARD_URL"),
		PollSeconds:    env("POLL_SECONDS", "20"),
		IdleSeconds:    env("IDLE_SECONDS", "900"),
	}}
	if err := r.SetupWithManager(mgr); err != nil {
		log.Error(err, "controller")
		os.Exit(1)
	}
	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)
	log.Info("agentdev-operator starting", "namespace", ns)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "run")
		os.Exit(1)
	}
}
