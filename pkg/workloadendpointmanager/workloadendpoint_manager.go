// Copyright 2022 Authors of spidernet-io
// SPDX-License-Identifier: Apache-2.0

package workloadendpointmanager

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apitypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/spidernet-io/spiderpool/pkg/constant"
	spiderpoolv1 "github.com/spidernet-io/spiderpool/pkg/k8s/apis/spiderpool.spidernet.io/v1"
	"github.com/spidernet-io/spiderpool/pkg/logutils"
	"github.com/spidernet-io/spiderpool/pkg/types"
)

type WorkloadEndpointManager interface {
	GetEndpointByName(ctx context.Context, namespace, podName string) (*spiderpoolv1.SpiderEndpoint, error)
	ListEndpoints(ctx context.Context, opts ...client.ListOption) (*spiderpoolv1.SpiderEndpointList, error)
	DeleteEndpoint(ctx context.Context, endpoint *spiderpoolv1.SpiderEndpoint) error
	RemoveFinalizer(ctx context.Context, namespace, podName string) error
	MarkIPAllocation(ctx context.Context, containerID string, pod *corev1.Pod, podController types.PodTopController) (*spiderpoolv1.SpiderEndpoint, error)
	ReMarkIPAllocation(ctx context.Context, containerID string, endpoint *spiderpoolv1.SpiderEndpoint, pod *corev1.Pod) error
	PatchIPAllocation(ctx context.Context, allocation *spiderpoolv1.PodIPAllocation, endpoint *spiderpoolv1.SpiderEndpoint) error
	ClearCurrentIPAllocation(ctx context.Context, containerID string, endpoint *spiderpoolv1.SpiderEndpoint) error
	ReallocateCurrentIPAllocation(ctx context.Context, containerID, nodeName string, endpoint *spiderpoolv1.SpiderEndpoint) error
}

type workloadEndpointManager struct {
	config EndpointManagerConfig
	client client.Client
}

func NewWorkloadEndpointManager(config EndpointManagerConfig, client client.Client) (WorkloadEndpointManager, error) {
	if client == nil {
		return nil, fmt.Errorf("k8s client %w", constant.ErrMissingRequiredParam)
	}

	return &workloadEndpointManager{
		config: setDefaultsForEndpointManagerConfig(config),
		client: client,
	}, nil
}

func (em *workloadEndpointManager) GetEndpointByName(ctx context.Context, namespace, podName string) (*spiderpoolv1.SpiderEndpoint, error) {
	var endpoint spiderpoolv1.SpiderEndpoint
	if err := em.client.Get(ctx, apitypes.NamespacedName{Namespace: namespace, Name: podName}, &endpoint); nil != err {
		return nil, err
	}

	return &endpoint, nil
}

func (em *workloadEndpointManager) ListEndpoints(ctx context.Context, opts ...client.ListOption) (*spiderpoolv1.SpiderEndpointList, error) {
	var endpointList spiderpoolv1.SpiderEndpointList
	if err := em.client.List(ctx, &endpointList, opts...); err != nil {
		return nil, err
	}

	return &endpointList, nil
}

func (em *workloadEndpointManager) DeleteEndpoint(ctx context.Context, endpoint *spiderpoolv1.SpiderEndpoint) error {
	if err := em.client.Delete(ctx, endpoint); err != nil {
		return client.IgnoreNotFound(err)
	}

	return nil
}

func (em *workloadEndpointManager) RemoveFinalizer(ctx context.Context, namespace, podName string) error {
	for i := 0; i <= em.config.MaxConflictRetries; i++ {
		endpoint, err := em.GetEndpointByName(ctx, namespace, podName)
		if err != nil {
			return client.IgnoreNotFound(err)
		}

		if !controllerutil.ContainsFinalizer(endpoint, constant.SpiderFinalizer) {
			return nil
		}

		controllerutil.RemoveFinalizer(endpoint, constant.SpiderFinalizer)
		if err := em.client.Update(ctx, endpoint); err != nil {
			if !apierrors.IsConflict(err) {
				return err
			}
			if i == em.config.MaxConflictRetries {
				return fmt.Errorf("%w (%d times), failed to remove finalizer %s from Endpoint %s/%s", constant.ErrRetriesExhausted, em.config.MaxConflictRetries, constant.SpiderFinalizer, namespace, podName)
			}
			time.Sleep(time.Duration(rand.Intn(1<<(i+1))) * em.config.ConflictRetryUnitTime)
			continue
		}
		break
	}

	return nil
}

func (em *workloadEndpointManager) MarkIPAllocation(ctx context.Context, containerID string, pod *corev1.Pod, podController types.PodTopController) (*spiderpoolv1.SpiderEndpoint, error) {
	if pod == nil {
		return nil, fmt.Errorf("pod %w", constant.ErrMissingRequiredParam)
	}

	logger := logutils.FromContext(ctx)

	endpoint := &spiderpoolv1.SpiderEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}

	// Do not set ownerReference for Endpoint when its corresponding Pod is
	// controlled by StatefulSet. Once the Pod of StatefulSet is recreated,
	// we can immediately retrieve the old IP allocation results from the
	// Endpoint without worrying about the cascading deletion of the Endpoint.
	if podController.Kind != constant.KindStatefulSet {
		if err := controllerutil.SetOwnerReference(pod, endpoint, em.config.scheme); err != nil {
			return nil, err
		}
	}
	controllerutil.AddFinalizer(endpoint, constant.SpiderFinalizer)

	logger.Sugar().Debugf("Create a new Endpoint %s/%s", endpoint.Namespace, endpoint.Name)
	if err := em.client.Create(ctx, endpoint); err != nil {
		return nil, err
	}

	allocation := &spiderpoolv1.PodIPAllocation{
		ContainerID:  containerID,
		Node:         &pod.Spec.NodeName,
		CreationTime: &metav1.Time{Time: time.Now()},
	}

	endpoint.Status.Current = allocation
	endpoint.Status.History = []spiderpoolv1.PodIPAllocation{*allocation}
	endpoint.Status.OwnerControllerType = podController.Kind
	endpoint.Status.OwnerControllerName = podController.Name

	logger.Sugar().Debugf("Update the current container ID of the new Endpoint %s/%s", endpoint.Namespace, endpoint.Name)
	if err := em.client.Status().Update(ctx, endpoint); err != nil {
		return nil, err
	}

	return endpoint, nil
}

func (em *workloadEndpointManager) ReMarkIPAllocation(ctx context.Context, containerID string, endpoint *spiderpoolv1.SpiderEndpoint, pod *corev1.Pod) error {
	if pod == nil {
		return fmt.Errorf("pod %w", constant.ErrMissingRequiredParam)
	}
	if endpoint == nil {
		return fmt.Errorf("endpoint %w", constant.ErrMissingRequiredParam)
	}

	logger := logutils.FromContext(ctx)

	// Create -> Delete -> Create a Pod with the same namespace and name in
	// a short time will cause some unexpected phenomena discussed in
	// https://github.com/spidernet-io/spiderpool/issues/1187.
	if endpoint.DeletionTimestamp != nil {
		// We can use GVK + Pod name (Same name as Endpoint) for more accurate
		// judgment, but this is unnecessary at present, because Endpoint has
		// only one Owner.
		ownerPod := endpoint.GetOwnerReferences()[0]
		// Beware of deleting the normal Endpoint manually.
		if ownerPod.UID != pod.GetUID() {
			return fmt.Errorf("currently, the IP addresses of the Pod %s/%s (uid: %s) is being recycled. You may create two Pods with the same namespace and name in a very short time", endpoint.Namespace, ownerPod.Name, string(ownerPod.UID))
		}
	}

	if endpoint.Status.Current != nil && endpoint.Status.Current.ContainerID == containerID {
		return nil
	}

	allocation := &spiderpoolv1.PodIPAllocation{
		ContainerID:  containerID,
		Node:         &pod.Spec.NodeName,
		CreationTime: &metav1.Time{Time: time.Now()},
	}

	endpoint.Status.Current = allocation
	endpoint.Status.History = append([]spiderpoolv1.PodIPAllocation{*allocation}, endpoint.Status.History...)
	if len(endpoint.Status.History) > *em.config.MaxHistoryRecords {
		logger.Sugar().Warnf("threshold of historical IP allocation records(<=%d) exceeded", em.config.MaxHistoryRecords)
		endpoint.Status.History = endpoint.Status.History[:*em.config.MaxHistoryRecords]
	}

	logger.Sugar().Debugf("Change the current container ID of the Endpoint %s/%s", endpoint.Namespace, endpoint.Name)

	return em.client.Status().Update(ctx, endpoint)
}

func (em *workloadEndpointManager) PatchIPAllocation(ctx context.Context, allocation *spiderpoolv1.PodIPAllocation, endpoint *spiderpoolv1.SpiderEndpoint) error {
	if endpoint == nil {
		return fmt.Errorf("endpoint %w", constant.ErrMissingRequiredParam)
	}

	if allocation == nil {
		return fmt.Errorf("allocation %w", constant.ErrMissingRequiredParam)
	}

	if endpoint.Status.Current == nil {
		return errors.New("patch a unmarked Endpoint")
	}

	if len(endpoint.Status.History) == 0 ||
		endpoint.Status.History[0].ContainerID != endpoint.Status.Current.ContainerID {
		return errors.New("data of the Endpoint is corrupt")
	}

	if endpoint.Status.Current.ContainerID != allocation.ContainerID {
		return errors.New("patch a mismarked Endpoint")
	}

	endpoint.Status.Current.IPs = allocation.IPs
	endpoint.Status.History = append([]spiderpoolv1.PodIPAllocation{*endpoint.Status.Current}, endpoint.Status.History...)

	return em.client.Status().Update(ctx, endpoint)
}

func (em *workloadEndpointManager) ClearCurrentIPAllocation(ctx context.Context, containerID string, endpoint *spiderpoolv1.SpiderEndpoint) error {
	if endpoint == nil || endpoint.Status.Current == nil {
		return nil
	}

	if endpoint.Status.Current.ContainerID != containerID {
		return nil
	}

	endpoint.Status.Current = nil
	if err := em.client.Status().Update(ctx, endpoint); err != nil {
		return client.IgnoreNotFound(err)
	}

	return nil
}

func (em *workloadEndpointManager) ReallocateCurrentIPAllocation(ctx context.Context, containerID, nodeName string, endpoint *spiderpoolv1.SpiderEndpoint) error {
	if endpoint == nil {
		return fmt.Errorf("endpoint %w", constant.ErrMissingRequiredParam)
	}

	if endpoint.Status.Current == nil {
		return errors.New("must be allocated befroe re-allocation")
	}

	if endpoint.Status.Current.ContainerID == containerID {
		return nil
	}

	endpoint.Status.Current.ContainerID = containerID
	*endpoint.Status.Current.Node = nodeName
	endpoint.Status.History = append([]spiderpoolv1.PodIPAllocation{*endpoint.Status.Current}, endpoint.Status.History...)

	return em.client.Status().Update(ctx, endpoint)
}
