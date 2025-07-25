/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/kubernetes-sigs/kernel-module-management/internal/constants"

	"github.com/kubernetes-sigs/kernel-module-management/internal/buildsign"
	"github.com/kubernetes-sigs/kernel-module-management/internal/mbsc"
	"github.com/kubernetes-sigs/kernel-module-management/internal/mic"
	"github.com/kubernetes-sigs/kernel-module-management/internal/node"
	"github.com/kubernetes-sigs/kernel-module-management/internal/pod"
	"github.com/kubernetes-sigs/kernel-module-management/internal/preflight"

	"github.com/kubernetes-sigs/kernel-module-management/api/v1beta1"
	"github.com/kubernetes-sigs/kernel-module-management/api/v1beta2"
	buildsignresource "github.com/kubernetes-sigs/kernel-module-management/internal/buildsign/resource"
	"github.com/kubernetes-sigs/kernel-module-management/internal/cmd"
	"github.com/kubernetes-sigs/kernel-module-management/internal/config"
	"github.com/kubernetes-sigs/kernel-module-management/internal/controllers"
	"github.com/kubernetes-sigs/kernel-module-management/internal/filter"
	"github.com/kubernetes-sigs/kernel-module-management/internal/metrics"
	"github.com/kubernetes-sigs/kernel-module-management/internal/module"
	"github.com/kubernetes-sigs/kernel-module-management/internal/nmc"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2/textlogger"
	clusterv1alpha1 "open-cluster-management.io/api/cluster/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	//+kubebuilder:scaffold:imports
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1beta1.AddToScheme(scheme))
	utilruntime.Must(v1beta2.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	logConfig := textlogger.NewConfig()
	logConfig.AddFlags(flag.CommandLine)

	var userConfigMapName string

	flag.StringVar(&userConfigMapName, "config", "", "Name of the ConfigMap containing user config.")

	flag.Parse()

	logger := textlogger.NewLogger(logConfig).WithName("kmm")

	ctrl.SetLogger(logger)

	setupLogger := logger.WithName("setup")
	operatorNamespace := cmd.GetEnvOrFatalError(constants.OperatorNamespaceEnvVar, setupLogger)

	commit, err := cmd.GitCommit()
	if err != nil {
		setupLogger.Error(err, "Could not get the git commit; using <undefined>")
		commit = "<undefined>"
	}

	workerImage := cmd.GetEnvOrFatalError("RELATED_IMAGE_WORKER", setupLogger)

	managed, err := GetBoolEnv("KMM_MANAGED")
	if err != nil {
		setupLogger.Error(err, "could not determine if we are running as managed; disabling")
		managed = false
	}

	setupLogger.Info("Creating manager", "git commit", commit)

	ctx := ctrl.SetupSignalHandler()
	cg := config.NewConfigGetter(setupLogger)

	cfg, err := cg.GetConfig(ctx, userConfigMapName, operatorNamespace, false)
	if err != nil {
		if errors.Is(err, config.ErrCannotUseCustomConfig) {
			setupLogger.Error(err, "failed to get kmm config")
		} else {
			cmd.FatalError(setupLogger, err, "failed to get kmm config")
		}
	}

	options := cg.GetManagerOptionsFromConfig(cfg, scheme)
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), options)
	if err != nil {
		cmd.FatalError(setupLogger, err, "unable to create manager")
	}

	client := mgr.GetClient()

	nmcHelper := nmc.NewHelper(client)
	filterAPI := filter.New(client, nmcHelper)

	metricsAPI := metrics.New()
	metricsAPI.Register()

	buildArgOverriderAPI := module.NewBuildArgOverrider()
	resourceManager := buildsignresource.NewResourceManager(client, buildArgOverriderAPI, scheme)
	nodeAPI := node.NewNode(client)
	kernelAPI := module.NewKernelMapper(buildArgOverriderAPI)
	micAPI := mic.New(client, scheme)
	mbscAPI := mbsc.New(client, scheme)
	imagePullerAPI := pod.NewImagePuller(client, scheme)

	dpc := controllers.NewDevicePluginReconciler(
		client,
		metricsAPI,
		filterAPI,
		nodeAPI,
		scheme)
	if err = dpc.SetupWithManager(mgr); err != nil {
		cmd.FatalError(setupLogger, err, "unable to create controller", "name", controllers.DevicePluginReconcilerName)
	}

	mnc := controllers.NewModuleReconciler(
		client,
		kernelAPI,
		nmcHelper,
		filterAPI,
		nodeAPI,
		micAPI,
		scheme,
	)
	if err = mnc.SetupWithManager(mgr); err != nil {
		cmd.FatalError(setupLogger, err, "unable to create controller", "name", controllers.ModuleReconcilerName)
	}

	eventRecorder := mgr.GetEventRecorderFor("kmm")

	workerPodManagerAPI := pod.NewWorkerPodManager(client, workerImage, scheme, &cfg.Worker)
	if err = controllers.NewNMCReconciler(client, scheme, workerImage, &cfg.Worker, eventRecorder, nodeAPI,
		workerPodManagerAPI).SetupWithManager(ctx, mgr); err != nil {

		cmd.FatalError(setupLogger, err, "unable to create controller", "name", controllers.NodeModulesConfigReconcilerName)
	}

	if err = controllers.NewDevicePluginPodReconciler(client).SetupWithManager(mgr); err != nil {
		cmd.FatalError(setupLogger, err, "unable to create controller", "name", controllers.DevicePluginPodReconcilerName)
	}

	if err = controllers.NewNodeLabelModuleVersionReconciler(client).SetupWithManager(mgr); err != nil {
		cmd.FatalError(setupLogger, err, "unable to create controller", "name", controllers.NodeLabelModuleVersionReconcilerName)
	}

	if err = controllers.NewMICReconciler(client, micAPI, mbscAPI, imagePullerAPI, scheme).SetupWithManager(mgr); err != nil {
		cmd.FatalError(setupLogger, err, "unable to create controller", "name", controllers.MICReconcilerName)
	}

	if managed {
		setupLogger.Info("Starting as managed")

		if err = clusterv1alpha1.Install(scheme); err != nil {
			cmd.FatalError(setupLogger, err, "could not add the Cluster API to the scheme")
		}

		if err = controllers.NewNodeKernelClusterClaimReconciler(client).SetupWithManager(mgr); err != nil {
			cmd.FatalError(setupLogger, err, "unable to create controller", "name", controllers.NodeKernelClusterClaimReconcilerName)
		}
	} else {
		builSignAPI := buildsign.NewManager(client, resourceManager, scheme)

		mbscr := controllers.NewMBSCReconciler(client, builSignAPI, mbscAPI)
		if err = mbscr.SetupWithManager(mgr); err != nil {
			cmd.FatalError(setupLogger, err, "unable to create controller", "name", controllers.MBSCReconcilerName)
		}

		helper := controllers.NewJobEventReconcilerHelper(client)

		if err = controllers.NewBuildSignEventsReconciler(client, helper, eventRecorder).SetupWithManager(mgr); err != nil {
			cmd.FatalError(setupLogger, err, "unable to create controller", "name", controllers.BuildSignEventsReconcilerName)
		}

		if err = controllers.NewJobGCReconciler(client, cfg.Job.GCDelay).SetupWithManager(mgr); err != nil {
			cmd.FatalError(setupLogger, err, "unable to create controller", "name", controllers.JobGCReconcilerName)
		}

		preflightAPI := preflight.NewPreflightAPI()

		if err = controllers.NewPreflightValidationReconciler(client, filterAPI, metricsAPI, micAPI, kernelAPI, preflightAPI).SetupWithManager(mgr); err != nil {
			cmd.FatalError(setupLogger, err, "unable to create controller", "name", controllers.PreflightValidationReconcilerName)
		}
	}

	//+kubebuilder:scaffold:builder

	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		cmd.FatalError(setupLogger, err, "unable to set up health check")
	}
	if err = mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		cmd.FatalError(setupLogger, err, "unable to set up ready check")
	}

	setupLogger.Info("starting manager")
	if err = mgr.Start(ctx); err != nil {
		cmd.FatalError(setupLogger, err, "problem running manager")
	}
}

func GetBoolEnv(s string) (bool, error) {
	envValue := os.Getenv(s)

	if envValue == "" {
		return false, nil
	}

	managed, err := strconv.ParseBool(envValue)
	if err != nil {
		return false, fmt.Errorf("%q: invalid value for %s", envValue, s)
	}

	return managed, nil
}
