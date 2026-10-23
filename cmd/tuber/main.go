package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ericeng/tuber/internal/config"
	"github.com/ericeng/tuber/internal/doit"
	"github.com/ericeng/tuber/internal/formatter"
	"github.com/ericeng/tuber/internal/server"
	"github.com/ericeng/tuber/internal/watcher"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

func main() {
	var (
		configPath     = flag.String("config", envOrDefault("TUBER_CONFIG", "/etc/tuber/config.yaml"), "Path to tuber YAML configuration file")
		kubeconfigPath = flag.String("kubeconfig", "", "Path to a single kubeconfig file (default: in-cluster credentials, then standard KUBECONFIG loading rules)")
		dryRunFlag     = flag.Bool("dry-run", false, "Log DoiT annotation payloads without sending HTTP POST requests")
		healthAddr     = flag.String("health-addr", ":8080", "Address for /healthz and /readyz HTTP probes")
		leaderElect    = flag.Bool("leader-elect", false, "Enable Kubernetes Lease leader election for HA deployments")
		leaderNS       = flag.String("leader-elect-namespace", envOrDefault("POD_NAMESPACE", "tuber-system"), "Namespace for the leader election Lease resource")
		leaderID       = flag.String("leader-elect-id", "tuber-leader-lock", "Name of the leader election Lease resource")
		validateOnly   = flag.Bool("validate-only", false, "Validate configuration file and exit")
		previewConfig  = flag.Bool("preview-config", false, "Preview effective settings and sample annotation text offline, excluding credentials")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if *dryRunFlag {
		_ = os.Setenv("TUBER_DRY_RUN", "true")
	}

	var (
		cfg *config.Config
		err error
	)
	if *previewConfig {
		cfg, err = config.LoadPreview(*configPath)
	} else if *validateOnly {
		cfg, err = config.LoadValidation(*configPath)
	} else {
		cfg, err = config.Load(*configPath)
	}
	if err != nil {
		logger.Error("configuration validation failed", "path", *configPath, "error", err,
			"hint", "Check the YAML field named in the error and environment overrides; use --preview-config to inspect effective settings. See README.md troubleshooting.")
		os.Exit(1)
	}

	if *previewConfig {
		if err := writeConfigPreview(os.Stdout, cfg); err != nil {
			logger.Error("configuration preview failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if *validateOnly {
		logger.Info("configuration is valid",
			"cluster", cfg.Cluster.Name,
			"provider", cfg.Cluster.Provider,
			"watchNamespaces", cfg.WatchNamespaces,
			"rules", len(cfg.Rules),
			"dryRun", cfg.DryRun,
			"note", "Offline check only; Kubernetes access, token validity, and report permissions have not been verified.",
		)
		return
	}

	k8sCfg, err := buildKubeConfig(*kubeconfigPath)
	if err != nil {
		logger.Error("failed to initialize Kubernetes client config", "error", err,
			"hint", "For local runs, set --kubeconfig to a readable kubeconfig file. In-cluster, check the pod ServiceAccount and mounted Kubernetes credentials.")
		os.Exit(1)
	}

	k8sClient, err := kubernetes.NewForConfig(k8sCfg)
	if err != nil {
		logger.Error("failed to create Kubernetes clientset", "error", err)
		os.Exit(1)
	}

	doitClient := doit.NewClient(cfg.DoiT, cfg.DryRun, doit.WithLogger(logger))
	if !cfg.DryRun && cfg.DoiT.ValidateOnStartupEnabled() {
		valCtx, cancelVal := context.WithTimeout(context.Background(), cfg.DoiT.Timeout)
		valResp, err := doitClient.ValidateServiceAccount(valCtx)
		cancelVal()
		if err != nil {
			logger.Error("DoiT Service Account token validation failed", "error", err,
				"hint", "Check the DoiT service account token, tenant ID, API reachability, and Cloud Analytics permissions. Use --preview-config for offline inspection.")
			os.Exit(1)
		}
		logger.Info("validated DoiT Service Account token",
			"domain", valResp.Domain,
			"permissions", valResp.Permissions,
		)
	}

	fmttr, err := formatter.New(cfg.DoiT)
	if err != nil {
		logger.Error("failed to initialize annotation formatter", "error", err)
		os.Exit(1)
	}

	mgr, err := watcher.NewManager(cfg, k8sClient, doitClient, fmttr, logger)
	if err != nil {
		logger.Error("failed to initialize watcher manager", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if strings.TrimSpace(*healthAddr) != "" {
		healthSrv := server.NewHealthServer(*healthAddr, mgr.Ready, logger)
		go func() {
			if err := healthSrv.Start(ctx); err != nil {
				logger.Error("health server exited with error", "error", err)
			}
		}()
	}

	if !*leaderElect {
		if err := mgr.Start(ctx); err != nil {
			logger.Error("watcher manager stopped with error", "error", err)
			os.Exit(1)
		}
		return
	}

	runWithLeaderElection(ctx, k8sClient, *leaderNS, *leaderID, logger, func(leaderCtx context.Context) {
		if err := mgr.Start(leaderCtx); err != nil {
			logger.Error("leader watcher manager stopped with error", "error", err)
		}
	})
}

func buildKubeConfig(kubeconfigPath string) (*rest.Config, error) {
	if strings.TrimSpace(kubeconfigPath) != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
}

func runWithLeaderElection(
	ctx context.Context,
	client kubernetes.Interface,
	namespace, lockName string,
	logger *slog.Logger,
	onStartedLeading func(context.Context),
) {
	id, err := os.Hostname()
	if err != nil || id == "" {
		id = fmt.Sprintf("tuber-%d", time.Now().UnixNano())
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      lockName,
			Namespace: namespace,
		},
		Client: client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: onStartedLeading,
			OnStoppedLeading: func() {
				logger.Info("lost leader election lease", "identity", id)
			},
			OnNewLeader: func(identity string) {
				logger.Info("observed leader election state", "leader", identity)
			},
		},
	})
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
