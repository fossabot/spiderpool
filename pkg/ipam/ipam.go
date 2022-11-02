// Copyright 2022 Authors of spidernet-io
// SPDX-License-Identifier: Apache-2.0

package ipam

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spidernet-io/spiderpool/api/v1/agent/models"
	"github.com/spidernet-io/spiderpool/pkg/constant"
	ippoolmanagertypes "github.com/spidernet-io/spiderpool/pkg/ippoolmanager/types"
	spiderpoolv1 "github.com/spidernet-io/spiderpool/pkg/k8s/apis/spiderpool.spidernet.io/v1"
	"github.com/spidernet-io/spiderpool/pkg/limiter"
	"github.com/spidernet-io/spiderpool/pkg/logutils"
	"github.com/spidernet-io/spiderpool/pkg/metric"
	"github.com/spidernet-io/spiderpool/pkg/namespacemanager"
	"github.com/spidernet-io/spiderpool/pkg/nodemanager"
	"github.com/spidernet-io/spiderpool/pkg/podmanager"
	"github.com/spidernet-io/spiderpool/pkg/statefulsetmanager"
	subnetmanagercontrollers "github.com/spidernet-io/spiderpool/pkg/subnetmanager/controllers"
	subnetmanagertypes "github.com/spidernet-io/spiderpool/pkg/subnetmanager/types"
	"github.com/spidernet-io/spiderpool/pkg/types"
	"github.com/spidernet-io/spiderpool/pkg/workloadendpointmanager"
)

type IPAM interface {
	Allocate(ctx context.Context, addArgs *models.IpamAddArgs) (*models.IpamAddResponse, error)
	Release(ctx context.Context, delArgs *models.IpamDelArgs) error
	Start(ctx context.Context) error
}

type ipam struct {
	config        *IPAMConfig
	ipamLimiter   limiter.Limiter
	ipPoolManager ippoolmanagertypes.IPPoolManager
	weManager     workloadendpointmanager.WorkloadEndpointManager
	nodeManager   nodemanager.NodeManager
	nsManager     namespacemanager.NamespaceManager
	podManager    podmanager.PodManager
	stsManager    statefulsetmanager.StatefulSetManager
	subnetManager subnetmanagertypes.SubnetManager
}

func NewIPAM(c *IPAMConfig, ipPoolManager ippoolmanagertypes.IPPoolManager, weManager workloadendpointmanager.WorkloadEndpointManager, nodeManager nodemanager.NodeManager,
	nsManager namespacemanager.NamespaceManager, podManager podmanager.PodManager, stsManager statefulsetmanager.StatefulSetManager, subnetMgr subnetmanagertypes.SubnetManager) (IPAM, error) {
	if c == nil {
		return nil, errors.New("ipam config must be specified")
	}
	if ipPoolManager == nil {
		return nil, errors.New("ippool manager must be specified")
	}
	if weManager == nil {
		return nil, errors.New("endpoint manager must be specified")
	}
	if nodeManager == nil {
		return nil, errors.New("node manager must be specified")
	}
	if nsManager == nil {
		return nil, errors.New("namespace manager must be specified")
	}
	if podManager == nil {
		return nil, errors.New("pod manager must be specified")
	}
	if stsManager == nil {
		return nil, errors.New("statefulset manager must be specified")
	}
	if c.EnableSpiderSubnet && subnetMgr == nil {
		return nil, errors.New("subnet manager must be specified")
	}

	return &ipam{
		config:        c,
		ipamLimiter:   limiter.NewLimiter(c.LimiterConfig),
		ipPoolManager: ipPoolManager,
		weManager:     weManager,
		nodeManager:   nodeManager,
		nsManager:     nsManager,
		podManager:    podManager,
		stsManager:    stsManager,
		subnetManager: subnetMgr,
	}, nil
}

func (i *ipam) Allocate(ctx context.Context, addArgs *models.IpamAddArgs) (*models.IpamAddResponse, error) {
	logger := logutils.FromContext(ctx)
	logger.Info("Start to allocate")

	pod, err := i.podManager.GetPodByName(ctx, *addArgs.PodNamespace, *addArgs.PodName)
	if err != nil {
		return nil, fmt.Errorf("failed to get Pod %s/%s: %v", *addArgs.PodNamespace, *addArgs.PodName, err)
	}
	podStatus, allocatable := podmanager.CheckPodStatus(pod)
	if !allocatable {
		return nil, fmt.Errorf("%s Pod %s/%s cannot allocate IP addresees", strings.ToLower(string(podStatus)), *addArgs.PodNamespace, *addArgs.PodName)
	}

	endpoint, err := i.weManager.GetEndpointByName(ctx, *addArgs.PodNamespace, *addArgs.PodName)
	if client.IgnoreNotFound(err) != nil {
		return nil, fmt.Errorf("failed to get Endpoint %s/%s: %v", *addArgs.PodNamespace, *addArgs.PodName, err)
	}

	ownerControllerType, ownerControllerName := podmanager.GetOwnerControllerType(pod)
	if i.config.EnableStatefulSet && ownerControllerType == constant.OwnerStatefulSet {
		addResp, err := i.retrieveStsIPAllocation(ctx, *addArgs.ContainerID, *addArgs.IfName, pod, endpoint)
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve the IP allocation of StatefulSet %s/%s: %w", *addArgs.PodNamespace, ownerControllerName, err)
		}
		if addResp != nil {
			return addResp, nil
		}
	} else {
		addResp, err := i.retrieveMultiNICIPAllocation(ctx, *addArgs.ContainerID, *addArgs.IfName, endpoint)
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve the IP allocation in multi-NIC mode: %w", err)
		}
		if addResp != nil {
			return addResp, nil
		}
	}

	addResp, err := i.allocateInStandardMode(ctx, addArgs, pod, endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate IP addresses in standard mode: %w", err)
	}

	return addResp, nil
}

func (i *ipam) retrieveStsIPAllocation(ctx context.Context, containerID, nic string, pod *corev1.Pod, endpoint *spiderpoolv1.SpiderEndpoint) (*models.IpamAddResponse, error) {
	logger := logutils.FromContext(ctx)

	if endpoint == nil {
		logger.Debug("Endpoint not found, do not retrieve anything for StatefulSet and try to allocate IP in standard mode")
		return nil, nil
	}

	// There's no possible that a StatefulSet Pod Endpoint's field 'Status.Current' is nil.
	if endpoint.Status.Current == nil {
		return nil, fmt.Errorf("current IP allocation is lost, endpoint %s/%s data broken: %+v", endpoint.Namespace, endpoint.Name, endpoint)
	}

	logger.Info("Retrieve the IP allocation of StatefulSet")
	nicMatched := false
	ips, routes := convertIPDetailsToIPConfigsAndAllRoutes(endpoint.Status.Current.IPs)
	for _, c := range ips {
		if *c.Nic == nic {
			if err := i.ipPoolManager.UpdateAllocatedIPs(ctx, containerID, pod, *c); err != nil {
				return nil, fmt.Errorf("failed to update StatefulSet's previous IP allocation records in IPPool %s: %w", c.IPPool, err)
			}
			nicMatched = true
		}
	}

	if !nicMatched {
		return nil, fmt.Errorf("nic %s do not match the current IP allocation of StatefulSet", nic)
	}

	// Refresh Endpoint current IP allocation.
	if err := i.weManager.UpdateCurrentStatus(ctx, containerID, pod); err != nil {
		return nil, fmt.Errorf("failed to update the current IP allocation of StatefulSet: %w", err)
	}

	addResp := &models.IpamAddResponse{
		Ips:    ips,
		Routes: routes,
	}
	logger.Sugar().Infof("Succeed to retrieve the IP allocation of StatefulSet: %+v", *addResp)

	return addResp, nil
}

func (i *ipam) retrieveMultiNICIPAllocation(ctx context.Context, containerID, nic string, endpoint *spiderpoolv1.SpiderEndpoint) (*models.IpamAddResponse, error) {
	logger := logutils.FromContext(ctx)
	logger.Debug("Try to retrieve the existing IP allocation in multi-NIC mode")

	allocation, _ := workloadendpointmanager.RetrieveIPAllocation(containerID, nic, false, endpoint)
	if allocation == nil {
		logger.Debug("Nothing retrieved to allocate")
		return nil, nil
	}

	ips, routes := convertIPDetailsToIPConfigsAndAllRoutes(allocation.IPs)
	addResp := &models.IpamAddResponse{
		Ips:    ips,
		Routes: routes,
	}
	logger.Sugar().Infof("Succeed to retrieve the IP allocation in multi-NIC mode: %+v", *addResp)

	return addResp, nil
}

func (i *ipam) allocateInStandardMode(ctx context.Context, addArgs *models.IpamAddArgs, pod *corev1.Pod, endpoint *spiderpoolv1.SpiderEndpoint) (*models.IpamAddResponse, error) {
	logger := logutils.FromContext(ctx)
	logger.Info("Allocate IP addresses in standard mode")

	toBeAllocatedSet, err := i.genToBeAllocatedSet(ctx, *addArgs.IfName, addArgs.DefaultIPV4IPPool, addArgs.DefaultIPV6IPPool, addArgs.CleanGateway, pod)
	if err != nil {
		return nil, err
	}

	// TODO(iiiceoo): Comment why containerID should be written first.
	endpoint, err = i.weManager.MarkIPAllocation(ctx, *addArgs.ContainerID, endpoint, pod)
	if err != nil {
		return nil, fmt.Errorf("failed to mark IP allocation: %v", err)
	}

	results, err := i.allocateForAllNICs(ctx, toBeAllocatedSet, *addArgs.ContainerID, endpoint, pod)
	resIPs, resRoutes := convertResultsToIPConfigsAndAllRoutes(results)
	if err != nil {
		// If there are any other errors that might have been thrown at Allocate
		// after the allocateForAllInterfaces is called, use defer.
		if len(resIPs) != 0 {
			if rollbackErr := i.release(ctx, *addArgs.ContainerID, convertResultsToIPDetails(results)); rollbackErr != nil {
				metric.IpamAllocationRollbackFailureCounts.Add(ctx, 1)
				logger.Sugar().Warnf("Failed to roll back the allocated IPs: %v", rollbackErr)
				return nil, err
			}
		}

		if err := i.weManager.ClearCurrentIPAllocation(ctx, *addArgs.ContainerID, endpoint); err != nil {
			logger.Sugar().Warnf("Failed to clear the current IP allocation: %v", err)
		}

		return nil, err
	}

	addResp := &models.IpamAddResponse{
		Ips:    resIPs,
		Routes: resRoutes,
	}
	logger.Sugar().Infof("Succeed to allocate: %+v", *addResp)

	return addResp, nil
}

func (i *ipam) genToBeAllocatedSet(ctx context.Context, nic string, defaultIPV4IPPool, defaultIPV6IPPool []string, cleanGateway bool, pod *corev1.Pod) ([]*ToBeAllocated, error) {
	logger := logutils.FromContext(ctx)

	preliminary, err := i.getPoolCandidates(ctx, nic, defaultIPV4IPPool, defaultIPV6IPPool, cleanGateway, pod)
	if err != nil {
		return nil, err
	}
	logger.Sugar().Infof("Preliminary IPPool candidates: %s", preliminary)

	if err := i.config.checkIPVersionEnable(ctx, preliminary); err != nil {
		return nil, err
	}

	if err := i.filterPoolCandidates(ctx, preliminary, pod); err != nil {
		return nil, err
	}
	logger.Sugar().Infof("Filtered IPPool candidates: %s", preliminary)

	if err := i.verifyPoolCandidates(ctx, preliminary); err != nil {
		return nil, err
	}
	logger.Info("All IPPool candidates valid")

	return preliminary, nil
}

func (i *ipam) allocateForAllNICs(ctx context.Context, tt []*ToBeAllocated, containerID string, endpoint *spiderpoolv1.SpiderEndpoint, pod *corev1.Pod) ([]*AllocationResult, error) {
	logger := logutils.FromContext(ctx)

	customRoutes, err := getCustomRoutes(ctx, pod)
	if err != nil {
		return nil, err
	}

	var allResults []*AllocationResult
	for _, t := range tt {
		oneResults, err := i.allocateForOneNIC(ctx, t, containerID, &customRoutes, endpoint, pod)
		if len(oneResults) != 0 {
			allResults = append(allResults, oneResults...)
		}
		if err != nil {
			return allResults, err
		}
	}
	if len(customRoutes) != 0 {
		logger.Sugar().Warnf("Invalid custom routes: %v", customRoutes)
	}

	ips, _ := convertResultsToIPConfigsAndAllRoutes(allResults)
	anno, err := genIPAssignmentAnnotation(ips)
	if err != nil {
		return allResults, fmt.Errorf("failed to generate IP assignment annotation: %v", err)
	}

	if err := i.podManager.MergeAnnotations(ctx, pod.Namespace, pod.Name, anno); err != nil {
		return allResults, fmt.Errorf("failed to merge IP assignment annotation: %w", err)
	}

	return allResults, nil
}

func (i *ipam) allocateForOneNIC(ctx context.Context, t *ToBeAllocated, containerID string, customRoutes *[]*models.Route, endpoint *spiderpoolv1.SpiderEndpoint, pod *corev1.Pod) ([]*AllocationResult, error) {
	var results []*AllocationResult
	for _, c := range t.PoolCandidates {
		result, err := i.allocateIPFromPoolCandidates(ctx, c, t.NIC, containerID, t.CleanGateway, pod)
		if result.IP != nil {
			results = append(results, result)
		}
		if err != nil {
			return results, err
		}

		routes, err := groupCustomRoutesByGW(ctx, customRoutes, result.IP)
		if err != nil {
			return results, fmt.Errorf("failed to group custom routes by gateway: %v", err)
		}
		result.Routes = append(result.Routes, routes...)

		patch := convertResultsToIPDetails([]*AllocationResult{result})
		if err = i.weManager.PatchIPAllocation(ctx, &spiderpoolv1.PodIPAllocation{
			ContainerID: containerID,
			IPs:         patch,
		}, endpoint); err != nil {
			return results, fmt.Errorf("failed to update IP allocation detail %+v of Endpoint %s/%s: %v", patch, endpoint.Namespace, endpoint.Name, err)
		}
	}

	return results, nil
}

func (i *ipam) allocateIPFromPoolCandidates(ctx context.Context, c *PoolCandidate, nic, containerID string, cleanGateway bool, pod *corev1.Pod) (*AllocationResult, error) {
	logger := logutils.FromContext(ctx)

	// TODO(iiiceoo): Comment why queue up before allocating IP.
	_, err := i.ipamLimiter.AcquireTicket(ctx, c.Pools...)
	if err != nil {
		logger.Sugar().Errorf("Failed to queue correctly: %v", err)
	} else {
		defer i.ipamLimiter.ReleaseTicket(ctx, c.Pools...)
	}

	var errs []error
	var usedIPPool *spiderpoolv1.SpiderIPPool
	result := &AllocationResult{}

	// TODO(iiiceoo): Debug panic https://github.com/spidernet-io/spiderpool/issues/974
	defer func() {
		if e := recover(); e != nil {
			logger.Sugar().Errorf("panic runtime error: AllocationResult: %+v", result)
			logger.Sugar().Errorf("panic runtime error: IPPool: %+v", usedIPPool)
			logger.Sugar().Errorf("%s", debug.Stack())
		}
	}()

	for _, pool := range c.Pools {
		ip, ipPool, err := i.ipPoolManager.AllocateIP(ctx, pool, containerID, nic, pod)
		if err != nil {
			errs = append(errs, err)
			logger.Sugar().Warnf("Failed to allocate IPv%d IP to %s from IPPool %s: %v", c.IPVersion, nic, pool, err)
			continue
		}
		usedIPPool = ipPool

		result.IP = ip
		result.CleanGateway = cleanGateway
		result.Routes = append(result.Routes, convertSpecRoutesToOAIRoutes(nic, ipPool.Spec.Routes)...)
		logger.Sugar().Infof("Allocate IPv%d IP %s to %s from IPPool %s", c.IPVersion, *result.IP.Address, nic, pool)
		break
	}

	if len(errs) == len(c.Pools) {
		return result, fmt.Errorf("failed to allocate any IPv%d IP address to %s from IPPools %v: %w", c.IPVersion, nic, c.Pools, utilerrors.NewAggregate(errs))
	}

	return result, nil
}

func (i *ipam) getPoolCandidates(ctx context.Context, nic string, netConfV4Pool, netConfV6Pool []string, cleanGateway bool, pod *corev1.Pod) ([]*ToBeAllocated, error) {
	// Select candidate IPPools through the Pod annotations "ipam.spidernet.io/subnets" or "ipam.spidernet.io/subnet"
	fromSubnet, err := i.getPoolFromSubnet(ctx, pod, nic, cleanGateway)
	if nil != err {
		return nil, fmt.Errorf("failed to get IPPool from SpiderSubnet, error: %v", err)
	}
	if fromSubnet != nil {
		return []*ToBeAllocated{fromSubnet}, nil
	}

	// Select candidate IPPools through the Pod annotation "ipam.spidernet.io/ippools".
	if anno, ok := pod.Annotations[constant.AnnoPodIPPools]; ok {
		return getPoolFromPodAnnoPools(ctx, anno, nic)
	}

	// Select candidate IPPools through the Pod annotation "ipam.spidernet.io/ippool".
	if anno, ok := pod.Annotations[constant.AnnoPodIPPool]; ok {
		t, err := getPoolFromPodAnnoPool(ctx, anno, nic, cleanGateway)
		if err != nil {
			return nil, err
		}
		return []*ToBeAllocated{t}, nil
	}

	// Select candidate IPPools through the Namespace annotations
	// "ipam.spidernet.io/defaultv4ippool" and "ipam.spidernet.io/defaultv6ippool".
	t, err := i.getPoolFromNS(ctx, pod.Namespace, nic, cleanGateway)
	if err != nil {
		return nil, err
	}
	if t != nil {
		return []*ToBeAllocated{t}, nil
	}

	// Select candidate IPPools through CNI network configuration.
	if t := getPoolFromNetConf(ctx, nic, netConfV4Pool, netConfV6Pool, cleanGateway); t != nil {
		return []*ToBeAllocated{t}, nil
	}

	// Select candidate IPPools through Configmap spiderpool-conf.
	t, err = i.config.getClusterDefaultPool(ctx, nic, cleanGateway)
	if err != nil {
		return nil, err
	}

	return []*ToBeAllocated{t}, nil
}

func (i *ipam) getPoolFromSubnet(ctx context.Context, pod *corev1.Pod, nic string, cleanGateway bool) (*ToBeAllocated, error) {
	subnetAnnoConfig, err := subnetmanagercontrollers.GetSubnetAnnoConfig(pod.Annotations)
	if nil != err {
		return nil, err
	}

	if subnetAnnoConfig == nil {
		// default IPAM mode
		return nil, nil
	}

	if i.config.EnableIPv4 && len(subnetAnnoConfig.SubnetName.IPv4) == 0 {
		return nil, fmt.Errorf("no specified IPv4 SpiderSubnet")
	}
	if i.config.EnableIPv6 && len(subnetAnnoConfig.SubnetName.IPv6) == 0 {
		return nil, fmt.Errorf("no specified IPv6 SpiderSubnet")
	}

	logger := logutils.FromContext(ctx)

	podTopControllerKind, podTopController, err := i.podManager.GetPodTopController(ctx, pod)
	if nil != err {
		return nil, fmt.Errorf("failed to get pod top owner reference, error: %v", err)
	}
	if podTopControllerKind == constant.OwnerNone || podTopControllerKind == constant.OwnerUnknown {
		return nil, fmt.Errorf("spider subnet doesn't support pod '%s/%s' owner controller", pod.Namespace, pod.Name)
	}

	t := &ToBeAllocated{
		NIC:          nic,
		CleanGateway: cleanGateway,
	}

	subnetName := subnetAnnoConfig.SubnetName

	// if enableIPv4 is off and get the specified SpiderSubnet IPv4 name, just filter it out
	if i.config.EnableIPv4 && len(subnetName.IPv4) != 0 {
		for j := 0; j <= i.config.WaitSubnetPoolRetries; j++ {
			v4PoolList, err := i.ipPoolManager.ListIPPools(ctx, client.MatchingLabels{
				constant.LabelIPPoolOwnerApplicationUID: string(podTopController.GetUID()),
				constant.LabelIPPoolVersion:             constant.LabelIPPoolVersionV4,
				constant.LabelIPPoolOwnerSpiderSubnet:   subnetName.IPv4[0],
				constant.LabelIPPoolOwnerApplication:    subnetmanagercontrollers.AppLabelValue(podTopControllerKind, podTopController.GetNamespace(), podTopController.GetName()),
			})
			if nil != err {
				if j == i.config.WaitSubnetPoolRetries {
					return nil, err
				}

				logger.Error(err.Error())
				continue
			}

			// if we do not get any IPv4 IPPool, we should wait for a second to let the spiderpool-controller app informers to create it.
			if v4PoolList == nil || len(v4PoolList.Items) == 0 {
				logger.Sugar().Errorf("no IPv4 IPPool retrieved from SpiderSubnet '%s', wait for a second and get a retry", subnetName.IPv4[0])

				time.Sleep(i.config.WaitSubnetPoolTime)
				continue
			} else if len(v4PoolList.Items) > 1 {
				// in the current mechanism, it's impossible to get multiple IPPools with the specified Labels, unless someone create it by hands.
				return nil, fmt.Errorf("it's invalid for '%s/%s/%s' corresponding SpiderSubnet '%s' owns multiple IPPools '%v' for one specify application",
					podTopControllerKind, podTopController.GetNamespace(), podTopController.GetName(), subnetName.IPv4[0], v4PoolList.Items)
			}

			logger.Sugar().Debugf("add IPv4 subnet IPPool '%s' to PoolCandidates", v4PoolList.Items[0].Name)
			t.PoolCandidates = append(t.PoolCandidates, &PoolCandidate{
				IPVersion: constant.IPv4,
				Pools:     []string{v4PoolList.Items[0].Name},
			})
			break
		}
	}

	// if enableIPv6 is off and get the specified SpiderSubnet IPv6 name, just filter it out
	if i.config.EnableIPv6 && len(subnetName.IPv6) != 0 {
		for j := 0; j <= i.config.WaitSubnetPoolRetries; j++ {
			v6PoolList, err := i.ipPoolManager.ListIPPools(ctx, client.MatchingLabels{
				constant.LabelIPPoolOwnerApplicationUID: string(podTopController.GetUID()),
				constant.LabelIPPoolVersion:             constant.LabelIPPoolVersionV6,
				constant.LabelIPPoolOwnerSpiderSubnet:   subnetName.IPv6[0],
				constant.LabelIPPoolOwnerApplication:    subnetmanagercontrollers.AppLabelValue(podTopControllerKind, podTopController.GetNamespace(), podTopController.GetName()),
			})
			if nil != err {
				if j == i.config.WaitSubnetPoolRetries {
					return nil, err
				}

				logger.Error(err.Error())
				continue
			}

			// if we do not get any IPv6 IPPool, we should wait for a second to let the spiderpool-controller app informers to create it.
			if v6PoolList == nil || len(v6PoolList.Items) == 0 {
				logger.Sugar().Errorf("no IPv6 IPPool retrieved from SpiderSubnet '%s', wait for a second and get a retry", subnetName.IPv6[0])

				time.Sleep(i.config.WaitSubnetPoolTime)
				continue
			} else if len(v6PoolList.Items) > 1 {
				// in the current mechanism, it's impossible to get multiple IPPools with the specified Labels, unless someone create it by hands.
				return nil, fmt.Errorf("it's invalid for '%s/%s/%s' corresponding SpiderSubnet '%s' owns multiple IPPools '%v' for one specify application",
					podTopControllerKind, podTopController.GetNamespace(), podTopController.GetName(), subnetName.IPv6[0], v6PoolList.Items)
			}

			logger.Sugar().Debugf("add IPv6 subnet IPPool '%s' to PoolCandidates", v6PoolList.Items[0].Name)
			t.PoolCandidates = append(t.PoolCandidates, &PoolCandidate{
				IPVersion: constant.IPv6,
				Pools:     []string{v6PoolList.Items[0].Name},
			})
			break
		}
	}

	return t, nil
}

func (i *ipam) getPoolFromNS(ctx context.Context, namespace, nic string, cleanGateway bool) (*ToBeAllocated, error) {
	logger := logutils.FromContext(ctx)

	ns, err := i.nsManager.GetNamespaceByName(ctx, namespace)
	if err != nil {
		return nil, err
	}
	nsDefaultV4Pools, nsDefaultV6Pools, err := i.nsManager.GetNSDefaultPools(ctx, ns)
	if err != nil {
		return nil, err
	}

	if len(nsDefaultV4Pools) == 0 && len(nsDefaultV6Pools) == 0 {
		return nil, nil
	}

	logger.Sugar().Infof("Use IPPools from Namespace annotation '%s'", constant.AnnotationPre+"/defaultv(4/6)ippool")
	t := &ToBeAllocated{
		NIC:          nic,
		CleanGateway: cleanGateway,
	}
	if len(nsDefaultV4Pools) != 0 {
		t.PoolCandidates = append(t.PoolCandidates, &PoolCandidate{
			IPVersion: constant.IPv4,
			Pools:     nsDefaultV4Pools,
		})
	}
	if len(nsDefaultV6Pools) != 0 {
		t.PoolCandidates = append(t.PoolCandidates, &PoolCandidate{
			IPVersion: constant.IPv6,
			Pools:     nsDefaultV6Pools,
		})
	}

	return t, nil
}

func (i *ipam) filterPoolCandidates(ctx context.Context, tt []*ToBeAllocated, pod *corev1.Pod) error {
	logger := logutils.FromContext(ctx)

	for _, t := range tt {
		for _, c := range t.PoolCandidates {
			var errs []error
			var selectedPools []string
			for _, pool := range c.Pools {
				if err := i.selectByPod(ctx, c.IPVersion, pool, pod); err != nil {
					errs = append(errs, err)
					logger.Sugar().Warnf("IPPool %s is filtered by Pod %s/%s: %v", pool, pod.Namespace, pod.Name, err)
					continue
				}
				selectedPools = append(selectedPools, pool)
			}

			if len(errs) == len(c.Pools) {
				return fmt.Errorf("%w, all IPv%d IPPools %v of %s filtered out: %v", constant.ErrNoAvailablePool, c.IPVersion, c.Pools, t.NIC, utilerrors.NewAggregate(errs))
			}
			c.Pools = selectedPools
		}
	}

	return nil
}

func (i *ipam) selectByPod(ctx context.Context, version types.IPVersion, poolName string, pod *corev1.Pod) error {
	ipPool, err := i.ipPoolManager.GetIPPoolByName(ctx, poolName)
	if err != nil {
		return fmt.Errorf("failed to get IPPool %s: %v", poolName, err)
	}

	if ipPool.DeletionTimestamp != nil {
		return fmt.Errorf("terminating IPPool %s", poolName)
	}

	if *ipPool.Spec.Disable {
		return fmt.Errorf("disabled IPPool %s", poolName)
	}

	if *ipPool.Spec.IPVersion != version {
		return fmt.Errorf("expect an IPv%d IPPool, but the version of the IPPool %s is IPv%d", version, poolName, *ipPool.Spec.IPVersion)
	}

	if ipPool.Status.TotalIPCount != nil && ipPool.Status.AllocatedIPCount != nil {
		if *ipPool.Status.TotalIPCount-*ipPool.Status.AllocatedIPCount == 0 {
			return constant.ErrIPUsedOut
		}
	}

	if ipPool.Spec.NodeAffinity != nil {
		nodeMatched, err := i.nodeManager.MatchLabelSelector(ctx, pod.Spec.NodeName, ipPool.Spec.NodeAffinity)
		if err != nil {
			return fmt.Errorf("failed to check the Node affinity of IPPool %s: %v", poolName, err)
		}
		if !nodeMatched {
			return fmt.Errorf("unmatched Node affinity of IPPool %s", poolName)
		}
	}

	if ipPool.Spec.NamespaceAffinity != nil {
		nsMatched, err := i.nsManager.MatchLabelSelector(ctx, pod.Namespace, ipPool.Spec.NamespaceAffinity)
		if err != nil {
			return fmt.Errorf("failed to check the Namespace affinity of IPPool %s: %v", poolName, err)
		}
		if !nsMatched {
			return fmt.Errorf("unmatched Namespace affinity of IPPool %s", poolName)
		}
	}

	if ipPool.Spec.PodAffinity != nil {
		podMatched, err := i.podManager.MatchLabelSelector(ctx, pod.Namespace, pod.Name, ipPool.Spec.PodAffinity)
		if err != nil {
			return fmt.Errorf("failed to check the Pod affinity of IPPool %s: %v", poolName, err)
		}
		if !podMatched {
			return fmt.Errorf("unmatched Pod affinity of IPPool %s", poolName)
		}
	}

	return nil
}

// TODO(iiiceoo): Refactor
func (i *ipam) verifyPoolCandidates(ctx context.Context, tt []*ToBeAllocated) error {
	for _, t := range tt {
		var allPools []string
		for _, c := range t.PoolCandidates {
			allPools = append(allPools, c.Pools...)
		}
		vlanToPools, same, err := i.ipPoolManager.CheckVlanSame(ctx, allPools)
		if err != nil {
			return err
		}
		if !same {
			return fmt.Errorf("%w, vlans in each IPPools are not same: %v", constant.ErrWrongInput, vlanToPools)
		}
	}

	return nil
}

func (i *ipam) Release(ctx context.Context, delArgs *models.IpamDelArgs) error {
	logger := logutils.FromContext(ctx)
	logger.Info("Start to release")

	pod, err := i.podManager.GetPodByName(ctx, *delArgs.PodNamespace, *delArgs.PodName)
	if err != nil {
		return fmt.Errorf("failed to get Pod %s/%s: %v", *delArgs.PodNamespace, *delArgs.PodName, err)
	}

	ownerControllerType, _ := podmanager.GetOwnerControllerType(pod)
	if i.config.EnableStatefulSet && ownerControllerType == constant.OwnerStatefulSet {
		isValidStsPod, err := i.stsManager.IsValidStatefulSetPod(ctx, pod.Namespace, pod.Name, ownerControllerType)
		if err != nil {
			return err
		}
		if isValidStsPod {
			logger.Info("No need to release the IP allocation of an StatefulSet whose scale is not reduced")
			return nil
		}
	}

	endpoint, err := i.weManager.GetEndpointByName(ctx, *delArgs.PodNamespace, *delArgs.PodName)
	if client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed to get Endpoint %s/%s: %v", *delArgs.PodNamespace, *delArgs.PodName, err)
	}
	allocation, currently := workloadendpointmanager.RetrieveIPAllocation(*delArgs.ContainerID, *delArgs.IfName, true, endpoint)
	if allocation == nil {
		logger.Info("Nothing retrieved for releasing")
		return nil
	}
	if !currently {
		logger.Warn("Request to release non current IP allocation, concurrency may exist between the same Pod")
	}

	if err = i.release(ctx, allocation.ContainerID, allocation.IPs); err != nil {
		return err
	}
	if err := i.weManager.ClearCurrentIPAllocation(ctx, *delArgs.ContainerID, endpoint); err != nil {
		return fmt.Errorf("failed to clear current IP allocation: %v", err)
	}
	logger.Sugar().Infof("Succeed to release: %+v", allocation.IPs)

	return nil
}

func (i *ipam) release(ctx context.Context, containerID string, details []spiderpoolv1.IPAllocationDetail) error {
	logger := logutils.FromContext(ctx)

	if len(details) == 0 {
		return nil
	}

	poolToIPAndCIDs := GroupIPDetails(containerID, details)
	errCh := make(chan error, len(poolToIPAndCIDs))
	wg := sync.WaitGroup{}
	wg.Add(len(poolToIPAndCIDs))

	for pool, ipAndCIDs := range poolToIPAndCIDs {
		go func(pool string, ipAndCIDs []types.IPAndCID) {
			defer wg.Done()

			_, err := i.ipamLimiter.AcquireTicket(ctx, pool)
			if err != nil {
				logger.Sugar().Errorf("Failed to queue correctly: %v", err)
			} else {
				defer i.ipamLimiter.ReleaseTicket(ctx, pool)
			}

			if err := i.ipPoolManager.ReleaseIP(ctx, pool, ipAndCIDs); err != nil {
				errCh <- err
				return
			}
			logger.Sugar().Infof("Succeed to release IP address %+v from IPPool %s", ipAndCIDs, pool)
		}(pool, ipAndCIDs)
	}
	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}

	if len(errs) != 0 {
		return fmt.Errorf("failed to release all allocated IP addresses %+v: %w", poolToIPAndCIDs, utilerrors.NewAggregate(errs))
	}

	return nil
}

func (i *ipam) Start(ctx context.Context) error {
	return i.ipamLimiter.Start(ctx)
}
