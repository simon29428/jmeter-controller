package main

import (
	"flag"
	"os"

	jmeterv1 "jmeter-controller/api/v1"
	"jmeter-controller/internal/apiserver"
	"jmeter-controller/internal/config"
	"jmeter-controller/internal/controller"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	_ "time/tzdata"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(jmeterv1.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr       string
		probeAddr         string
		configPath        string
		apiPort           int
		enableLeaderElect bool
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8081", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8082", "The address the health probe endpoint binds to.")
	flag.StringVar(&configPath, "config", "", "Path to the controller config YAML file.")
	flag.IntVar(&apiPort, "api-port", 8080, "Port for the REST API server.")
	flag.BoolVar(&enableLeaderElect, "leader-elect", false, "Enable leader election for high availability.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	// Load controller config
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		setupLog.Error(err, "Failed to load controller config")
		os.Exit(1)
	}
	setupLog.Info("Loaded controller config", "runGroupLimits", cfg.RunGroupLimits)

	// Create the manager
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElect,
		LeaderElectionID:       "jmeter-controller-leader",
	})
	if err != nil {
		setupLog.Error(err, "Unable to create manager")
		os.Exit(1)
	}

	// Register TestRun controller
	if err := (&controller.TestRunReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Config: cfg,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to set up TestRun controller")
		os.Exit(1)
	}

	// Health checks
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up ready check")
		os.Exit(1)
	}

	// SetupSignalHandler must be called exactly once; share the ctx between
	// the manager and the API server.
	ctx := ctrl.SetupSignalHandler()

	// Start embedded REST API server in a goroutine
	apiSrv := apiserver.New(mgr.GetClient(), apiPort, ctrl.Log.WithName("apiserver"))
	go func() {
		if err := apiSrv.Start(ctx); err != nil {
			setupLog.Error(err, "REST API server error")
		}
	}()

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Problem running manager")
		os.Exit(1)
	}
}
