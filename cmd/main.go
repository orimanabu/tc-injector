// main is the entrypoint for the tc-injector DaemonSet controller.
package main

import (
	"flag"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	tcv1alpha1 "github.com/tc-injector/tc-injector/pkg/api/v1alpha1"
	"github.com/tc-injector/tc-injector/pkg/controller"
	"github.com/tc-injector/tc-injector/pkg/veth"
	// corev1 is registered in init(); the import is needed for the side effect.
	_ "k8s.io/client-go/kubernetes/scheme"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(tcv1alpha1.AddToScheme(scheme))
	_ = corev1.AddToScheme(scheme)
}

func main() {
	var (
		metricsAddr     string
		probeAddr       string
		criSocket       string
		nodeName        string
		leaderElect     bool
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address for the metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for health probes.")
	flag.StringVar(&criSocket, "cri-socket", "",
		"Path to the CRI socket (as mounted from the host). "+
			"If empty, auto-detects containerd or CRI-O socket.")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"),
		"Name of the Kubernetes node this pod is running on.")
	flag.BoolVar(&leaderElect, "leader-elect", false,
		"Enable leader election (not needed for a DaemonSet).")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	logger := zap.New(zap.UseFlagOptions(&opts))
	ctrl.SetLogger(logger)

	setupLog := logger.WithName("setup")

	if nodeName == "" {
		setupLog.Error(nil, "NODE_NAME environment variable or --node-name flag is required")
		os.Exit(1)
	}

	finder, err := veth.NewFinder(criSocket)
	if err != nil {
		setupLog.Error(err, "failed to create veth finder")
		os.Exit(1)
	}
	defer finder.Close()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "tc-injector.setns.net",
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	reconciler := &controller.Reconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		NodeName:  nodeName,
		Finder:    finder,
		TCApplier: controller.RealTCApplier{},
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up readiness check")
		os.Exit(1)
	}

	setupLog.Info("starting tc-injector", "node", nodeName)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
