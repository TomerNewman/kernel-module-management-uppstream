package cluster

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	hubv1beta1 "github.com/kubernetes-sigs/kernel-module-management/api-hub/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kmmv1beta1 "github.com/kubernetes-sigs/kernel-module-management/api/v1beta1"
	"github.com/kubernetes-sigs/kernel-module-management/internal/api"
	"github.com/kubernetes-sigs/kernel-module-management/internal/build"
	"github.com/kubernetes-sigs/kernel-module-management/internal/constants"
	"github.com/kubernetes-sigs/kernel-module-management/internal/module"
	"github.com/kubernetes-sigs/kernel-module-management/internal/pod"
	"github.com/kubernetes-sigs/kernel-module-management/internal/sign"
)

//go:generate mockgen -source=cluster.go -package=cluster -destination=mock_cluster.go

type ClusterAPI interface {
	RequestedManagedClusterModule(ctx context.Context, namespacedName types.NamespacedName) (*hubv1beta1.ManagedClusterModule, error)
	SelectedManagedClusters(ctx context.Context, mcm *hubv1beta1.ManagedClusterModule) (*clusterv1.ManagedClusterList, error)
	BuildAndSign(ctx context.Context, mcm hubv1beta1.ManagedClusterModule, cluster clusterv1.ManagedCluster) (bool, error)
	GarbageCollectBuildsAndSigns(ctx context.Context, mcm hubv1beta1.ManagedClusterModule) ([]string, error)
	KernelVersions(cluster clusterv1.ManagedCluster) ([]string, error)
}

type clusterAPI struct {
	client    client.Client
	kernelAPI module.KernelMapper
	buildAPI  build.Manager
	signAPI   sign.SignManager
	namespace string
}

func NewClusterAPI(
	client client.Client,
	kernelAPI module.KernelMapper,
	buildAPI build.Manager,
	signAPI sign.SignManager,
	defaultPodNamespace string) ClusterAPI {
	return &clusterAPI{
		client:    client,
		kernelAPI: kernelAPI,
		buildAPI:  buildAPI,
		signAPI:   signAPI,
		namespace: defaultPodNamespace,
	}
}

func (c *clusterAPI) RequestedManagedClusterModule(
	ctx context.Context,
	namespacedName types.NamespacedName) (*hubv1beta1.ManagedClusterModule, error) {

	mcm := hubv1beta1.ManagedClusterModule{}
	if err := c.client.Get(ctx, namespacedName, &mcm); err != nil {
		return nil, fmt.Errorf("failed to get the ManagedClusterModule %s: %w", namespacedName, err)
	}
	return &mcm, nil
}

func (c *clusterAPI) SelectedManagedClusters(
	ctx context.Context,
	mcm *hubv1beta1.ManagedClusterModule) (*clusterv1.ManagedClusterList, error) {

	clusterList := &clusterv1.ManagedClusterList{}

	opts := []client.ListOption{
		client.MatchingLabelsSelector{
			Selector: labels.Set(mcm.Spec.Selector).AsSelector(),
		},
	}

	err := c.client.List(ctx, clusterList, opts...)

	return clusterList, err
}

func (c *clusterAPI) BuildAndSign(
	ctx context.Context,
	mcm hubv1beta1.ManagedClusterModule,
	cluster clusterv1.ManagedCluster) (bool, error) {

	modSpec := mcm.Spec.ModuleSpec
	mod := kmmv1beta1.Module{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mcm.Name,
			Namespace: c.namespace,
		},
		Spec: modSpec,
	}
	mldMappings, err := c.kernelMappingsByKernelVersion(ctx, &mod, cluster)
	if err != nil {
		return false, err
	}

	// if no mappings were found, return not completed
	if len(mldMappings) == 0 {
		return false, nil
	}

	logger := log.FromContext(ctx)

	completedSuccessfully := true
	for kernelVersion, mld := range mldMappings {
		buildCompleted, err := c.build(ctx, mld, &mcm)
		if err != nil {
			return false, err
		}

		kernelVersionLogger := logger.WithValues(
			"kernel version", kernelVersion,
		)

		if !buildCompleted {
			kernelVersionLogger.Info("Build for mapping has not completed yet, skipping Sign")
			completedSuccessfully = false
			continue
		}

		signCompleted, err := c.sign(ctx, mld, &mcm)
		if err != nil {
			return false, err
		}

		if !signCompleted {
			kernelVersionLogger.Info("Sign for mapping has not completed yet")
			completedSuccessfully = false
			continue
		}
	}

	return completedSuccessfully, nil
}

func (c *clusterAPI) GarbageCollectBuildsAndSigns(ctx context.Context, mcm hubv1beta1.ManagedClusterModule) ([]string, error) {
	deletedBuilds, err := c.buildAPI.GarbageCollect(ctx, mcm.Name, c.namespace, &mcm)
	if err != nil {
		return nil, fmt.Errorf("failed to garbage collect build object: %v", err)
	}

	deletedSigns, err := c.signAPI.GarbageCollect(ctx, mcm.Name, c.namespace, &mcm)
	if err != nil {
		return nil, fmt.Errorf("failed to garbage collect sign object: %v", err)
	}
	return append(deletedBuilds, deletedSigns...), nil
}

func (c *clusterAPI) KernelVersions(cluster clusterv1.ManagedCluster) ([]string, error) {
	for _, clusterClaim := range cluster.Status.ClusterClaims {
		if clusterClaim.Name != constants.KernelVersionsClusterClaimName {
			continue
		}

		kernelVersions := strings.Split(clusterClaim.Value, "\n")
		sort.Strings(kernelVersions)

		return kernelVersions, nil
	}
	return nil, errors.New("KMM kernel version ClusterClaim not found")
}

func (c *clusterAPI) kernelMappingsByKernelVersion(
	ctx context.Context,
	mod *kmmv1beta1.Module,
	cluster clusterv1.ManagedCluster) (map[string]*api.ModuleLoaderData, error) {

	kernelVersions, err := c.KernelVersions(cluster)
	if err != nil {
		return nil, err
	}

	mldMappings := make(map[string]*api.ModuleLoaderData)
	logger := log.FromContext(ctx)

	for _, kernelVersion := range kernelVersions {
		kernelVersion := strings.TrimSuffix(kernelVersion, "+")

		kernelVersionLogger := logger.WithValues(
			"kernel version", kernelVersion,
		)

		if mld, ok := mldMappings[kernelVersion]; ok {
			kernelVersionLogger.V(1).Info("Using cached mld mapping", "mld", mld)
			continue
		}

		mld, err := c.kernelAPI.GetModuleLoaderDataForKernel(mod, kernelVersion)
		if err != nil {
			kernelVersionLogger.Info("no suitable container image found; skipping kernel version")
			continue
		}

		kernelVersionLogger.V(1).Info("Found a valid mapping",
			"image", mld.ContainerImage,
			"build", mld.Build != nil,
		)

		mldMappings[kernelVersion] = mld
	}

	return mldMappings, nil
}

func (c *clusterAPI) build(
	ctx context.Context,
	mld *api.ModuleLoaderData,
	mcm *hubv1beta1.ManagedClusterModule) (bool, error) {

	shouldSync, err := c.buildAPI.ShouldSync(ctx, mld)
	if err != nil {
		return false, fmt.Errorf("could not check if build synchronization is needed: %v", err)
	}
	if !shouldSync {
		return true, nil
	}

	logger := log.FromContext(ctx).WithValues(
		"kernel version", mld.KernelVersion,
		"image", mld.ContainerImage)
	buildCtx := log.IntoContext(ctx, logger)

	buildStatus, err := c.buildAPI.Sync(buildCtx, mld, true, mcm)
	if err != nil {
		return false, fmt.Errorf("could not synchronize the build: %w", err)
	}

	if buildStatus == pod.StatusCompleted {
		return true, nil
	}
	return false, nil
}

func (c *clusterAPI) sign(
	ctx context.Context,
	mld *api.ModuleLoaderData,
	mcm *hubv1beta1.ManagedClusterModule) (bool, error) {

	shouldSync, err := c.signAPI.ShouldSync(ctx, mld)
	if err != nil {
		return false, fmt.Errorf("could not check if signing synchronization is needed: %v", err)
	}
	if !shouldSync {
		return true, nil
	}

	// if we need to sign AND we've built, then we must have built
	// the intermediate image so must figure out its name
	previousImage := ""
	if module.ShouldBeBuilt(mld) {
		previousImage = module.IntermediateImageName(mld.Name, mld.Namespace, mld.ContainerImage)
	}

	logger := log.FromContext(ctx).WithValues(
		"kernel version", mld.KernelVersion,
		"image", mld.ContainerImage)
	signCtx := log.IntoContext(ctx, logger)

	signStatus, err := c.signAPI.Sync(signCtx, mld, previousImage, true, mcm)
	if err != nil {
		return false, fmt.Errorf("could not synchronize the signing: %w", err)
	}

	if signStatus == pod.StatusCompleted {
		return true, nil
	}
	return false, nil
}
