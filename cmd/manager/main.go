package main

import (
	"flag"
	"os"
	"time"

	api "github.com/GProjectdev/Karmada_with_PVMigration/api/v1alpha1"
	"github.com/GProjectdev/Karmada_with_PVMigration/internal/management"
	"github.com/GProjectdev/Karmada_with_PVMigration/internal/member"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	var mode, clusterName, metricsAddr, probeAddr, leaderNamespace string
	var leader bool
	var interval time.Duration
	flag.StringVar(&mode, "mode", "", "management or member")
	flag.StringVar(&clusterName, "cluster-name", "", "Karmada member cluster name (member mode)")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "metrics address; 0 disables")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe address")
	flag.StringVar(&leaderNamespace, "leader-election-namespace", "pv-migration-system", "lease namespace in the configured API server")
	flag.BoolVar(&leader, "leader-elect", true, "enable leader election")
	flag.DurationVar(&interval, "poll-interval", 15*time.Second, "reconciliation interval")
	zapOptions := zap.Options{}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))
	log := ctrl.Log.WithName("setup")
	if mode != "management" && mode != "member" {
		log.Error(nil, "--mode must be management or member")
		os.Exit(1)
	}
	if mode == "member" && clusterName == "" {
		log.Error(nil, "--cluster-name is required in member mode")
		os.Exit(1)
	}
	if interval < time.Second {
		log.Error(nil, "--poll-interval must be at least one second")
		os.Exit(1)
	}
	// controller-runtime registers --kubeconfig. Management must explicitly use Karmada.
	kubeconfig := flag.Lookup("kubeconfig").Value.String()
	if mode == "management" && kubeconfig == "" {
		log.Error(nil, "management requires explicit --kubeconfig pointing to Karmada")
		os.Exit(1)
	}
	cfg, err := ctrl.GetConfig()
	if mode == "management" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		log.Error(err, "load Kubernetes config")
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	if err = clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err = api.AddToScheme(scheme); err != nil {
		panic(err)
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: metricsAddr}, HealthProbeBindAddress: probeAddr, LeaderElection: leader, LeaderElectionID: "pv-migration-" + mode + ".migration.dcnlab.com", LeaderElectionNamespace: leaderNamespace})
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}
	if mode == "member" {
		err = (&member.PVSyncReconciler{Client: mgr.GetClient(), ClusterName: clusterName, PollInterval: interval}).SetupWithManager(mgr)
	} else {
		err = (&management.MetadataDiscovery{Client: mgr.GetClient(), PollInterval: interval}).SetupWithManager(mgr)
		if err == nil {
			err = (&management.PVMigrationReconciler{Client: mgr.GetClient(), PollInterval: interval}).SetupWithManager(mgr)
		}
		if err == nil {
			err = (&management.PVCleanupReconciler{Client: mgr.GetClient(), PollInterval: interval}).SetupWithManager(mgr)
		}
	}
	if err != nil {
		log.Error(err, "register controllers")
		os.Exit(1)
	}
	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		panic(err)
	}
	if err = mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		panic(err)
	}
	if err = mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "run manager")
		os.Exit(1)
	}
}
