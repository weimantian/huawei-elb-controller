// Command huawei-elb-controller is a Kubernetes controller that manages Huawei
// Cloud ELBs for OpenEverest V1 (Percona Everest). It provides two reconcilers:
//
//  1. Service Reconciler (Plan 2): watches LoadBalancer Services, injects
//     CCE autocreate annotations for ELB creation, and updates ELB parameters
//     when LBC annotations change.
//  2. LoadBalancerConfig Reconciler (legacy): watches LoadBalancerConfig CRs
//     and creates/deletes ELBs via the Huawei Cloud ELB v3 API.
package main

import (
	"flag"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/weimantian/huawei-elb-controller/internal/controller"
	"github.com/weimantian/huawei-elb-controller/internal/huaweicloud"
)

func main() {
	var metricsAddr string
	var devMode bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8081",
		"Bind address for the metrics endpoint.")
	flag.BoolVar(&devMode, "zap-dev-mode", false,
		"Enable zap development mode (verbose, colored logs). Defaults to false for production.")
	flag.Parse()

	logger := zap.New(zap.UseDevMode(devMode))
	ctrl.SetLogger(logger)

	// 1. Load Huawei Cloud credentials from environment variables.
	creds, err := huaweicloud.LoadCredentials()
	if err != nil {
		logger.Error(err, "failed to load Huawei Cloud credentials")
		os.Exit(1)
	}

	// 2. Build the ELB v3 client.
	elbClient, err := huaweicloud.NewELBClient(creds)
	if err != nil {
		logger.Error(err, "failed to create Huawei Cloud ELB client")
		os.Exit(1)
	}
	networkDetector := huaweicloud.NewNetworkDetector(creds)
	// 3. Get in-cluster Kubernetes config.
	kubeConfig, err := ctrl.GetConfig()
	if err != nil {
		logger.Error(err, "failed to get Kubernetes config")
		os.Exit(1)
	}

	// 4. Create the controller-runtime manager.
	mgr, err := ctrl.NewManager(kubeConfig, ctrl.Options{
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: ":8082",
	})
	if err != nil {
		logger.Error(err, "failed to start manager")
		os.Exit(1)
	}

	// 5. Register the Service Reconciler (Plan 2 — primary reconciler).
	//    Watches LoadBalancer Services, injects autocreate annotations for ELB
	//    creation, and handles parameter updates via Huawei Cloud ELB API.
	if err := (&controller.ServiceReconciler{
		Client:          mgr.GetClient(),
		ELBClient:       elbClient,
		NetworkDetector: networkDetector,
		Creds:           creds,
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "failed to setup Service Reconciler")
		os.Exit(1)
	}

	// 6. Register the LoadBalancerConfig Reconciler (legacy — for existing LBC resources).
	if err := (&controller.LoadBalancerConfigReconciler{
		Client:          mgr.GetClient(),
		ELBClient:       elbClient,
		Creds:           creds,
		NetworkDetector: networkDetector,
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "failed to setup LBC Reconciler")
		os.Exit(1)
	}

	// 6b. Register health/readiness checks so /healthz and /readyz
	// are served by the health probe server on :8082.
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	logger.Info("starting huawei-elb-controller (Plan 2: Service Reconciler + legacy LBC Reconciler)",
		"region", creds.Region, "metrics", metricsAddr)

	// 6. Run until SIGTERM/SIGINT.
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
