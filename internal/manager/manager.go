// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"flag"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"gitlab.com/kubesan/kubesan/internal/common/commands"
	"gitlab.com/kubesan/kubesan/internal/common/config"
	clustercontrollers "gitlab.com/kubesan/kubesan/internal/manager/cluster"
	nodecontrollers "gitlab.com/kubesan/kubesan/internal/manager/node"
)

var (
	setupLog = ctrl.Log.WithName("setup")
)

func RunClusterControllers() error {
	var enableLeaderElection bool
	var probeAddr string
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.IntVar(&config.MaxConcurrentReconciles, "max-concurrent-reconciles", 10, "The number of objects that a given controller can reconcile at once.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	ctrlOpts := ctrl.Options{
		Scheme: config.Scheme,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				config.Namespace: {},
			},
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "dbe08e41." + config.Domain,
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		LeaderElectionReleaseOnCancel: true,
	}

	return runManager(ctrlOpts, []func(ctrl.Manager) error{
		clustercontrollers.SetUpSnapshotReconciler,
		clustercontrollers.SetUpThinPoolLvReconciler,
		clustercontrollers.SetUpVolumeReconciler,
	})
}

func RunNodeControllers() error {
	var probeAddr string
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.IntVar(&config.MaxConcurrentReconciles, "max-concurrent-reconciles", 10, "The number of objects that a given controller can reconcile at once.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	ctrlOpts := ctrl.Options{
		Scheme: config.Scheme,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				config.Namespace: {},
			},
		},
		HealthProbeBindAddress: probeAddr,
	}

	return runManager(ctrlOpts, []func(ctrl.Manager) error{
		nodecontrollers.SetUpNBDExportNodeReconciler,
		nodecontrollers.SetUpThinPoolLvNodeReconciler,
		nodecontrollers.SetUpVolumeNodeReconciler,
	})
}

func runManager(ctrlOpts ctrl.Options, controllerSetUpFuncs []func(ctrl.Manager) error) error {
	// KubeSAN VGs use their own LVM profile to avoid interfering with the system-wide lvm.conf config. This profile
	// is hardcoded here and is put in place before creating LVs that get their config from the profile.
	err := commands.LvmCreateProfile(config.LvmProfileName, config.LvmProfile)
	if err != nil {
		return err
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrlOpts)
	if err != nil {
		return err
	}

	for _, f := range controllerSetUpFuncs {
		if err := f(mgr); err != nil {
			return err
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return err
	}

	return nil
}
