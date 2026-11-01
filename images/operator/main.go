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
	// executor Job helpers (#69): the same binary hashes and verifies plan files
	if code, ok := planHelper(os.Args[1:], os.Stdout); ok {
		os.Exit(code)
	}
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)

	ns := env("AGENTS_NS", "coding-agents")
	execNS := os.Getenv("EXEC_NS") // deterministic executors (#69); empty: off
	namespaces := map[string]cache.Config{ns: {}}
	if execNS != "" {
		namespaces[execNS] = cache.Config{}
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cache.Options{DefaultNamespaces: namespaces}, // agents (and executors) namespace only
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: ":8081", // one replica, no leader election (add a Lease Role first if that changes)
	})
	if err != nil {
		log.Error(err, "manager")
		os.Exit(1)
	}
	r := &AgentReconciler{Client: mgr.GetClient(), D: Defaults{
		WorkerImage:     env("WORKER_IMAGE", "ghcr.io/valentinalexeev/agentdev-worker:latest"),
		ForgeImage:      env("FORGE_IMAGE", "ghcr.io/valentinalexeev/agentdev-forge:latest"),
		PullPolicy:      env("IMAGE_PULL_POLICY", "IfNotPresent"),
		ForgeURL:        os.Getenv("FORGE_URL"),
		ForgePublicURL:  os.Getenv("FORGE_PUBLIC_URL"),
		GitHubURL:       env("GITHUB_URL", "https://github.com"),
		Reviewer:        os.Getenv("REVIEWER"),
		BotboardURL:     os.Getenv("BOTBOARD_URL"),
		PollSeconds:     env("POLL_SECONDS", "20"),
		IdleSeconds:     env("IDLE_SECONDS", "900"),
		Approvals:       env("APPROVALS", "false"),
		ApprovalsURL:    os.Getenv("APPROVALS_URL"),
		ApprovalTimeout: env("APPROVAL_TIMEOUT_MINUTES", "30"),
		EgressProxy:     os.Getenv("EGRESS_PROXY"),
		ClaudeProxyURL:  os.Getenv("CLAUDE_PROXY_URL"),
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
	// subscription usage limits (#93): fed by the collector, re-evaluated every 30 s (thresholds, window resets)
	DefaultFiveHour = envPct("USAGE_FIVE_HOUR", DefaultFiveHour)
	DefaultWeekly = envPct("USAGE_WEEKLY", DefaultWeekly)
	usage := &Usage{Client: mgr.GetClient(), Direct: mgr.GetAPIReader(), NS: ns, Now: time.Now}
	if err := mgr.Add(usage); err != nil {
		log.Error(err, "usage")
		os.Exit(1)
	}
	col := &Collector{Client: mgr.GetClient(), Direct: mgr.GetAPIReader(), NS: ns, Logs: kubeLogs(cs), Now: time.Now, Usage: usage,
		ProxyUsage: os.Getenv("CLAUDE_PROXY_URL") != "",
		C: SecurityConfig{
			PauseAfter:        envInt("SECURITY_PAUSE_AFTER", 5),
			Retention:         time.Duration(envInt("SECURITY_RETENTION_DAYS", 30)) * 24 * time.Hour,
			MaxPerAgent:       envInt("SECURITY_MAX_PER_AGENT", 500),
			RatePerMin:        envInt("SECURITY_RATE_PER_MIN", 10),
			RateBurst:         envInt("SECURITY_RATE_BURST", 20),
			ApprovalRetention: time.Duration(envInt("APPROVAL_RETENTION_DAYS", 90)) * 24 * time.Hour,
			Rescan:            15 * time.Second,
		}}
	if err := mgr.Add(col); err != nil {
		log.Error(err, "security collector")
		os.Exit(1)
	}
	if execNS != "" {
		if execNS == ns {
			log.Error(nil, "EXEC_NS must not be the agents namespace: write credentials never live next to agents")
			os.Exit(1)
		}
		ex := &ExecutorPoller{Client: mgr.GetClient(), Direct: mgr.GetAPIReader(), Logs: kubePodLogs(cs), Now: time.Now,
			C: ExecConfig{NS: execNS, AgentsNS: ns, HelperImage: env("OPERATOR_IMAGE", "ghcr.io/valentinalexeev/agentdev-operator:latest"),
				GitImage: env("FORGE_IMAGE", "ghcr.io/valentinalexeev/agentdev-forge:latest"), PullPolicy: env("IMAGE_PULL_POLICY", "IfNotPresent"),
				ForgeURL: os.Getenv("FORGE_URL"), GitHubAPI: os.Getenv("GITHUB_API_URL"), GitHubURL: os.Getenv("GITHUB_URL"),
				Poll:       time.Duration(envInt("EXECUTOR_POLL_SECONDS", 60)) * time.Second,
				Lookback:   time.Duration(envInt("EXECUTOR_LOOKBACK_HOURS", 24)) * time.Hour,
				Retention:  time.Duration(envInt("EXECUTOR_RETENTION_DAYS", 30)) * 24 * time.Hour,
				Storage:    env("EXECUTOR_PLAN_STORAGE", "1Gi"),
				ApplyStart: time.Duration(envInt("EXECUTOR_APPLY_START_MINUTES", 30)) * time.Minute}}
		if err := mgr.Add(ex); err != nil {
			log.Error(err, "executor poller")
			os.Exit(1)
		}
		log.Info("deterministic executors on", "namespace", execNS)
	}
	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)
	log.Info("agentdev-operator starting", "namespace", ns)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "run")
		os.Exit(1)
	}
}

// envPct reads a percent 1..100; any other value keeps def.
func envPct(k string, def int) int {
	if n := envInt(k, def); n >= 1 && n <= 100 {
		return n
	}
	return def
}
