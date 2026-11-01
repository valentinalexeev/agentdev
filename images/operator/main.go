// agentdev operator (#47): reconciles Agent resources (agentdev.dev/v1alpha1) into agent StatefulSets and Services.
// Day-2 lifecycle (add, remove, pause) moves here from scripts; cluster bootstrap stays outside (#4).
package main

import (
	"flag"
	"os"
	"strconv"
	"time"

	"k8s.io/client-go/kubernetes"

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

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil && n > 0 {
		return n
	}
	return def
}

func main() {
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
		HealthProbeBindAddress: ":8081", // one replica, no leader election (add a Lease Role first if that changes)
	})
	if err != nil {
		log.Error(err, "manager")
		os.Exit(1)
	}
	r := &AgentReconciler{Client: mgr.GetClient(), D: Defaults{
		WorkerImage:    env("WORKER_IMAGE", "ghcr.io/valentinalexeev/agentdev-worker:latest"),
		ForgeImage:     env("FORGE_IMAGE", "ghcr.io/valentinalexeev/agentdev-forge:latest"),
		PullPolicy:     env("IMAGE_PULL_POLICY", "IfNotPresent"),
		ForgeURL:       os.Getenv("FORGE_URL"),
		ForgePublicURL: os.Getenv("FORGE_PUBLIC_URL"),
		Reviewer:       os.Getenv("REVIEWER"),
		BotboardURL:    os.Getenv("BOTBOARD_URL"),
		PollSeconds:    env("POLL_SECONDS", "20"),
		IdleSeconds:    env("IDLE_SECONDS", "900"),
		EgressProxy:    os.Getenv("EGRESS_PROXY"),
	}}
	if err := r.SetupWithManager(mgr); err != nil {
		log.Error(err, "controller")
		os.Exit(1)
	}
	// security events (#70): follow agent pod logs, record SecurityEvents, auto-pause
	cs, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		log.Error(err, "clientset")
		os.Exit(1)
	}
	col := &Collector{Client: mgr.GetClient(), Direct: mgr.GetAPIReader(), NS: ns, Logs: kubeLogs(cs), Now: time.Now,
		C: SecurityConfig{
			PauseAfter:  envInt("SECURITY_PAUSE_AFTER", 5),
			Retention:   time.Duration(envInt("SECURITY_RETENTION_DAYS", 30)) * 24 * time.Hour,
			MaxPerAgent: envInt("SECURITY_MAX_PER_AGENT", 500),
			RatePerMin:  envInt("SECURITY_RATE_PER_MIN", 10),
			RateBurst:   envInt("SECURITY_RATE_BURST", 20),
			Rescan:      15 * time.Second,
		}}
	if err := mgr.Add(col); err != nil {
		log.Error(err, "security collector")
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
