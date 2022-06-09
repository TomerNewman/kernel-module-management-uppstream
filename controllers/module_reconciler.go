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

package controllers

import (
	"context"
	"fmt"

	ootov1alpha1 "github.com/qbarrand/oot-operator/api/v1alpha1"
	"github.com/qbarrand/oot-operator/controllers/build"
	"github.com/qbarrand/oot-operator/controllers/module"
	"github.com/qbarrand/oot-operator/controllers/predicates"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// ModuleReconciler reconciles a Module object
type ModuleReconciler struct {
	client.Client

	bm build.Manager
	dc DaemonSetCreator
	km module.KernelMapper
	su module.ConditionsUpdater
}

func NewModuleReconciler(
	client client.Client,
	bm build.Manager,
	dg DaemonSetCreator,
	km module.KernelMapper,
	su module.ConditionsUpdater,
) *ModuleReconciler {
	return &ModuleReconciler{
		Client: client,
		bm:     bm,
		dc:     dg,
		km:     km,
		su:     su,
	}
}

//+kubebuilder:rbac:groups=ooto.sigs.k8s.io,resources=modules,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ooto.sigs.k8s.io,resources=modules/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ooto.sigs.k8s.io,resources=modules/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=create;delete;get;list;patch;watch
//+kubebuilder:rbac:groups="core",resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups="batch",resources=jobs,verbs=create;list;watch

// Reconcile lists all nodes and looks for kernels that match its mappings.
// For each mapping that matches at least one node in the cluster, it creates a DaemonSet running the container image
// on the nodes with a compatible kernel.
func (r *ModuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	res := ctrl.Result{}

	logger := log.FromContext(ctx)

	mod := ootov1alpha1.Module{}

	if err := r.Client.Get(ctx, req.NamespacedName, &mod); err != nil {
		logger.Error(err, "Could not get module")
		return res, err
	}

	logger.V(1).Info("Listing nodes", "selector", mod.Spec.Selector)

	nodes := v1.NodeList{}

	opt := client.MatchingLabels(mod.Spec.Selector)

	if err := r.Client.List(ctx, &nodes, opt); err != nil {
		logger.Error(err, "Could not list nodes; retrying")
		return res, fmt.Errorf("could not list nodes: %v", err)
	}

	mappings := r.getKernelMappings(ctx, nodes, &mod)

	dsByKernelVersion, err := r.dc.ModuleDaemonSetsByKernelVersion(ctx, mod)
	if err != nil {
		return res, fmt.Errorf("could get DaemonSets for module %s: %v", mod.Name, err)
	}

	//// TODO qbarrand: find a better place for this
	//if err := r.su.SetAsReady(ctx, &mod, "TODO", "TODO"); err != nil {
	//	return res, fmt.Errorf("could not set the initial conditions: %v", err)
	//}

	for kernelVersion, m := range mappings {
		requeue, err := r.handleBuild(ctx, &mod, m, kernelVersion)
		if err != nil {
			return res, fmt.Errorf("failed to handle build for kernel version %s: %w", kernelVersion, err)
		}
		if requeue {
			logger.Info("Build requires a requeue; skipping handling driver container for now", "kernelVersion", kernelVersion, "image", m)
			res.Requeue = true
			continue
		}

		err = r.handleDriverContainer(ctx, &mod, m, dsByKernelVersion, kernelVersion)
		if err != nil {
			return res, fmt.Errorf("failed to handle driver container for kernel version %s: %v", kernelVersion, err)
		}
	}

	logger.Info("Handle device plugin")
	err = r.handleDevicePlugin(ctx, &mod)
	if err != nil {
		return res, fmt.Errorf("could handle device plugin: %w", err)
	}

	logger.Info("Garbage-collecting DaemonSets")

	// Garbage collect old DaemonSets for which there are no nodes.
	validKernels := sets.StringKeySet(mappings)

	deleted, err := r.dc.GarbageCollect(ctx, dsByKernelVersion, validKernels)
	if err != nil {
		return res, fmt.Errorf("could not garbage collect DaemonSets: %v", err)
	}

	logger.Info("Garbage-collected DaemonSets", "names", deleted)

	return res, nil
}

func (r *ModuleReconciler) getKernelMappings(ctx context.Context, nodes v1.NodeList, mod *ootov1alpha1.Module) map[string]*ootov1alpha1.KernelMapping {
	mappings := make(map[string]*ootov1alpha1.KernelMapping)
	logger := log.FromContext(ctx)

	for _, node := range nodes.Items {
		osConfig := r.km.GetNodeOSConfig(&node)
		kernelVersion := node.Status.NodeInfo.KernelVersion

		nodeLogger := logger.WithValues(
			"node", node.Name,
			"kernel version", kernelVersion,
		)

		if image, ok := mappings[kernelVersion]; ok {
			nodeLogger.V(1).Info("Using cached image", "image", image)
			continue
		}

		m, err := r.km.FindMappingForKernel(mod.Spec.KernelMappings, kernelVersion)
		if err != nil {
			nodeLogger.Info("no suitable container image found; skipping node")
			continue
		}

		m, err = r.km.PrepareKernelMapping(m, osConfig)
		if err != nil {
			nodeLogger.Info("failed to substitute the template variables in the mapping", "error", err)
			continue
		}

		nodeLogger.V(1).Info("Found a valid mapping",
			"image", m.ContainerImage,
			"build", m.Build != nil,
		)

		mappings[kernelVersion] = m
	}
	return mappings
}

func (r *ModuleReconciler) handleBuild(ctx context.Context,
	mod *ootov1alpha1.Module,
	km *ootov1alpha1.KernelMapping,
	kernelVersion string) (bool, error) {
	if km.Build == nil {
		return false, nil
	}

	// TODO check access to the image - execute build only if needed
	logger := log.FromContext(ctx).WithValues("kernel version", kernelVersion, "image", km)
	buildCtx := log.IntoContext(ctx, logger)

	buildRes, err := r.bm.Sync(buildCtx, *mod, *km, kernelVersion)
	if err != nil {
		return false, fmt.Errorf("could not synchronize the build: %w", err)
	}

	return buildRes.Requeue, nil
}

func (r *ModuleReconciler) handleDriverContainer(ctx context.Context,
	mod *ootov1alpha1.Module,
	km *ootov1alpha1.KernelMapping,
	dsByKernelVersion map[string]*appsv1.DaemonSet,
	kernelVersion string) error {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: mod.Namespace},
	}

	logger := log.FromContext(ctx)
	if existingDS := dsByKernelVersion[kernelVersion]; existingDS != nil {
		logger.Info("updating existing driver container DS", "kernel version", kernelVersion, "image", km, "name", ds.Name)
		ds = existingDS
	} else {
		logger.Info("creating new driver container DS", "kernel version", kernelVersion, "image", km)
		ds.GenerateName = mod.Name + "-"
	}

	_, err := controllerutil.CreateOrPatch(ctx, r.Client, ds, func() error {
		return r.dc.SetDriverContainerAsDesired(ctx, ds, km.ContainerImage, *mod, kernelVersion)
	})

	return err
}

func (r *ModuleReconciler) handleDevicePlugin(ctx context.Context, mod *ootov1alpha1.Module) error {
	if mod.Spec.DevicePlugin == nil {
		return nil
	}

	logger := log.FromContext(ctx)
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: mod.Namespace},
	}
	name := mod.Name + "-device-plugin"
	ds.Name = name
	err := r.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: mod.Namespace}, ds)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get the device plugin daemonset %s/%s: %w", name, mod.Namespace, err)
	}

	opRes, err := controllerutil.CreateOrPatch(ctx, r.Client, ds, func() error {
		return r.dc.SetDevicePluginAsDesired(ctx, ds, mod)
	})

	if err == nil {
		logger.Info("Reconciled Device Plugin", "name", ds.Name, "result", opRes)
	}

	return err
}

// SetupWithManager sets up the controller with the Manager.
func (r *ModuleReconciler) SetupWithManager(mgr ctrl.Manager, kernelLabel string) error {
	nmm := NewNodeModuleMapper(
		r.Client,
		mgr.GetLogger().WithName("controller/module/node-module-mapper"),
	)

	return ctrl.NewControllerManagedBy(mgr).
		For(&ootov1alpha1.Module{}).
		Owns(&appsv1.DaemonSet{}).
		Owns(&batchv1.Job{}).
		Watches(
			&source.Kind{Type: &v1.Node{}},
			handler.EnqueueRequestsFromMapFunc(nmm.FindModulesForNode),
			builder.WithPredicates(
				ModuleReconcilerNodePredicate(kernelLabel),
			),
		).
		Named("module").
		Complete(r)
}

func ModuleReconcilerNodePredicate(kernelLabel string) predicate.Predicate {
	return predicate.And(
		predicates.SkipDeletions,
		predicates.HasLabel(kernelLabel),
		predicate.LabelChangedPredicate{},
	)
}
