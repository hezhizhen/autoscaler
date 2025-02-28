/*
Copyright 2016 The Kubernetes Authors.

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

package core

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/autoscaler/cluster-autoscaler/core/scaledown/pdb"
	"k8s.io/autoscaler/cluster-autoscaler/core/scaledown/planner"
	scaledownstatus "k8s.io/autoscaler/cluster-autoscaler/core/scaledown/status"
	"k8s.io/autoscaler/cluster-autoscaler/core/scaleup"
	orchestrator "k8s.io/autoscaler/cluster-autoscaler/core/scaleup/orchestrator"
	"k8s.io/autoscaler/cluster-autoscaler/debuggingsnapshot"
	"k8s.io/autoscaler/cluster-autoscaler/utils/taints"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/clusterstate"
	"k8s.io/autoscaler/cluster-autoscaler/clusterstate/utils"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	"k8s.io/autoscaler/cluster-autoscaler/context"
	"k8s.io/autoscaler/cluster-autoscaler/core/scaledown"
	"k8s.io/autoscaler/cluster-autoscaler/core/scaledown/actuation"
	"k8s.io/autoscaler/cluster-autoscaler/core/scaledown/deletiontracker"
	"k8s.io/autoscaler/cluster-autoscaler/core/scaledown/legacy"
	core_utils "k8s.io/autoscaler/cluster-autoscaler/core/utils"
	"k8s.io/autoscaler/cluster-autoscaler/estimator"
	"k8s.io/autoscaler/cluster-autoscaler/expander"
	"k8s.io/autoscaler/cluster-autoscaler/metrics"
	ca_processors "k8s.io/autoscaler/cluster-autoscaler/processors"
	"k8s.io/autoscaler/cluster-autoscaler/processors/status"
	"k8s.io/autoscaler/cluster-autoscaler/simulator"
	"k8s.io/autoscaler/cluster-autoscaler/simulator/clustersnapshot"
	"k8s.io/autoscaler/cluster-autoscaler/simulator/predicatechecker"
	"k8s.io/autoscaler/cluster-autoscaler/utils/backoff"
	caerrors "k8s.io/autoscaler/cluster-autoscaler/utils/errors"
	scheduler_utils "k8s.io/autoscaler/cluster-autoscaler/utils/scheduler"
	"k8s.io/autoscaler/cluster-autoscaler/utils/tpu"
	"k8s.io/utils/integer"

	klog "k8s.io/klog/v2"
)

const (
	// How old the oldest unschedulable pod should be before starting scale up.
	unschedulablePodTimeBuffer = 2 * time.Second
	// How old the oldest unschedulable pod with GPU should be before starting scale up.
	// The idea is that nodes with GPU are very expensive and we're ready to sacrifice
	// a bit more latency to wait for more pods and make a more informed scale-up decision.
	unschedulablePodWithGpuTimeBuffer = 30 * time.Second

	// NodeUpcomingAnnotation is an annotation CA adds to nodes which are upcoming.
	NodeUpcomingAnnotation = "cluster-autoscaler.k8s.io/upcoming-node"

	// podScaleUpDelayAnnotationKey is an annotation how long pod can wait to be scaled up.
	podScaleUpDelayAnnotationKey = "cluster-autoscaler.kubernetes.io/pod-scale-up-delay"
)

// StaticAutoscaler is an autoscaler which has all the core functionality of a CA but without the reconfiguration feature
type StaticAutoscaler struct {
	// AutoscalingContext consists of validated settings and options for this autoscaler
	*context.AutoscalingContext
	// ClusterState for maintaining the state of cluster nodes.
	clusterStateRegistry    *clusterstate.ClusterStateRegistry
	lastScaleUpTime         time.Time
	lastScaleDownDeleteTime time.Time
	lastScaleDownFailTime   time.Time
	scaleDownPlanner        scaledown.Planner
	scaleDownActuator       scaledown.Actuator
	scaleUpOrchestrator     scaleup.Orchestrator
	processors              *ca_processors.AutoscalingProcessors
	processorCallbacks      *staticAutoscalerProcessorCallbacks
	initialized             bool
	ignoredTaints           taints.TaintKeySet
}

type staticAutoscalerProcessorCallbacks struct {
	disableScaleDownForLoop bool
	extraValues             map[string]interface{}
	scaleDownPlanner        scaledown.Planner
}

func (callbacks *staticAutoscalerProcessorCallbacks) ResetUnneededNodes() {
	callbacks.scaleDownPlanner.CleanUpUnneededNodes()
}

func newStaticAutoscalerProcessorCallbacks() *staticAutoscalerProcessorCallbacks {
	callbacks := &staticAutoscalerProcessorCallbacks{}
	callbacks.reset()
	return callbacks
}

func (callbacks *staticAutoscalerProcessorCallbacks) DisableScaleDownForLoop() {
	callbacks.disableScaleDownForLoop = true
}

func (callbacks *staticAutoscalerProcessorCallbacks) SetExtraValue(key string, value interface{}) {
	callbacks.extraValues[key] = value
}

func (callbacks *staticAutoscalerProcessorCallbacks) GetExtraValue(key string) (value interface{}, found bool) {
	value, found = callbacks.extraValues[key]
	return
}

func (callbacks *staticAutoscalerProcessorCallbacks) reset() {
	callbacks.disableScaleDownForLoop = false
	callbacks.extraValues = make(map[string]interface{})
}

// NewStaticAutoscaler creates an instance of Autoscaler filled with provided parameters
func NewStaticAutoscaler(
	opts config.AutoscalingOptions,
	predicateChecker predicatechecker.PredicateChecker,
	clusterSnapshot clustersnapshot.ClusterSnapshot,
	autoscalingKubeClients *context.AutoscalingKubeClients,
	processors *ca_processors.AutoscalingProcessors,
	cloudProvider cloudprovider.CloudProvider,
	expanderStrategy expander.Strategy,
	estimatorBuilder estimator.EstimatorBuilder,
	backoff backoff.Backoff,
	debuggingSnapshotter debuggingsnapshot.DebuggingSnapshotter,
	remainingPdbTracker pdb.RemainingPdbTracker,
	scaleUpOrchestrator scaleup.Orchestrator) *StaticAutoscaler {

	processorCallbacks := newStaticAutoscalerProcessorCallbacks()
	autoscalingContext := context.NewAutoscalingContext(
		opts,
		predicateChecker,
		clusterSnapshot,
		autoscalingKubeClients,
		cloudProvider,
		expanderStrategy,
		estimatorBuilder,
		processorCallbacks,
		debuggingSnapshotter,
		remainingPdbTracker)

	clusterStateConfig := clusterstate.ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: opts.MaxTotalUnreadyPercentage,
		OkTotalUnreadyCount:       opts.OkTotalUnreadyCount,
		MaxNodeProvisionTime:      opts.MaxNodeProvisionTime,
	}

	ignoredTaints := make(taints.TaintKeySet)
	for _, taintKey := range opts.IgnoredTaints {
		klog.V(4).Infof("Ignoring taint %s on all NodeGroups", taintKey)
		ignoredTaints[taintKey] = true
	}

	clusterStateRegistry := clusterstate.NewClusterStateRegistry(autoscalingContext.CloudProvider, clusterStateConfig, autoscalingContext.LogRecorder, backoff)
	processors.ScaleDownCandidatesNotifier.Register(clusterStateRegistry)

	deleteOptions := simulator.NodeDeleteOptions{
		SkipNodesWithSystemPods:   opts.SkipNodesWithSystemPods,
		SkipNodesWithLocalStorage: opts.SkipNodesWithLocalStorage,
		MinReplicaCount:           opts.MinReplicaCount,
	}

	// TODO: Populate the ScaleDownActuator/Planner fields in AutoscalingContext
	// during the struct creation rather than here.
	ndt := deletiontracker.NewNodeDeletionTracker(0 * time.Second)
	scaleDown := legacy.NewScaleDown(autoscalingContext, processors, ndt, deleteOptions)
	actuator := actuation.NewActuator(autoscalingContext, clusterStateRegistry, ndt, deleteOptions)
	autoscalingContext.ScaleDownActuator = actuator

	var scaleDownPlanner scaledown.Planner
	var scaleDownActuator scaledown.Actuator
	if opts.ParallelDrain {
		scaleDownPlanner = planner.New(autoscalingContext, processors, deleteOptions)
		scaleDownActuator = actuator
	} else {
		// TODO: Remove the wrapper once the legacy implementation becomes obsolete.
		scaleDownWrapper := legacy.NewScaleDownWrapper(scaleDown, actuator)
		scaleDownPlanner = scaleDownWrapper
		scaleDownActuator = scaleDownWrapper
	}
	processorCallbacks.scaleDownPlanner = scaleDownPlanner

	if scaleUpOrchestrator == nil {
		scaleUpOrchestrator = orchestrator.New()
	}
	scaleUpOrchestrator.Initialize(autoscalingContext, processors, clusterStateRegistry, ignoredTaints)

	// Set the initial scale times to be less than the start time so as to
	// not start in cooldown mode.
	initialScaleTime := time.Now().Add(-time.Hour)
	return &StaticAutoscaler{
		AutoscalingContext:      autoscalingContext,
		lastScaleUpTime:         initialScaleTime,
		lastScaleDownDeleteTime: initialScaleTime,
		lastScaleDownFailTime:   initialScaleTime,
		scaleDownPlanner:        scaleDownPlanner,
		scaleDownActuator:       scaleDownActuator,
		scaleUpOrchestrator:     scaleUpOrchestrator,
		processors:              processors,
		processorCallbacks:      processorCallbacks,
		clusterStateRegistry:    clusterStateRegistry,
		ignoredTaints:           ignoredTaints,
	}
}

// Start starts components running in background.
func (a *StaticAutoscaler) Start() error {
	a.clusterStateRegistry.Start()
	return nil
}

// cleanUpIfRequired removes ToBeDeleted taints added by a previous run of CA
// the taints are removed only once per runtime
func (a *StaticAutoscaler) cleanUpIfRequired() {
	if a.initialized {
		return
	}

	// CA can die at any time. Removing taints that might have been left from the previous run.
	if allNodes, err := a.AllNodeLister().List(); err != nil {
		klog.Errorf("Failed to list ready nodes, not cleaning up taints: %v", err)
	} else {
		taints.CleanAllToBeDeleted(allNodes,
			a.AutoscalingContext.ClientSet, a.Recorder, a.CordonNodeBeforeTerminate)
		if a.AutoscalingContext.AutoscalingOptions.MaxBulkSoftTaintCount == 0 {
			// Clean old taints if soft taints handling is disabled
			taints.CleanAllDeletionCandidates(allNodes,
				a.AutoscalingContext.ClientSet, a.Recorder)
		}
	}
	a.initialized = true
}

func (a *StaticAutoscaler) initializeClusterSnapshot(nodes []*apiv1.Node, scheduledPods []*apiv1.Pod) caerrors.AutoscalerError {
	a.ClusterSnapshot.Clear()

	knownNodes := make(map[string]bool)
	for _, node := range nodes {
		if err := a.ClusterSnapshot.AddNode(node); err != nil {
			klog.Errorf("Failed to add node %s to cluster snapshot: %v", node.Name, err)
			return caerrors.ToAutoscalerError(caerrors.InternalError, err)
		}
		knownNodes[node.Name] = true
	}
	for _, pod := range scheduledPods {
		if knownNodes[pod.Spec.NodeName] {
			if err := a.ClusterSnapshot.AddPod(pod, pod.Spec.NodeName); err != nil {
				klog.Errorf("Failed to add pod %s scheduled to node %s to cluster snapshot: %v", pod.Name, pod.Spec.NodeName, err)
				return caerrors.ToAutoscalerError(caerrors.InternalError, err)
			}
		}
	}
	return nil
}

func (a *StaticAutoscaler) initializeRemainingPdbTracker() caerrors.AutoscalerError {
	a.RemainingPdbTracker.Clear()

	pdbs, err := a.PodDisruptionBudgetLister().List()
	if err != nil {
		klog.Errorf("Failed to list pod disruption budgets: %v", err)
		return caerrors.NewAutoscalerError(caerrors.ApiCallError, err.Error())
	}
	err = a.RemainingPdbTracker.SetPdbs(pdbs)
	if err != nil {
		return caerrors.NewAutoscalerError(caerrors.InternalError, err.Error())
	}
	return nil
}

// RunOnce iterates over node groups and scales them up/down if necessary
func (a *StaticAutoscaler) RunOnce(currentTime time.Time) caerrors.AutoscalerError {
	a.cleanUpIfRequired()
	a.processorCallbacks.reset()
	a.clusterStateRegistry.PeriodicCleanup()
	a.DebuggingSnapshotter.StartDataCollection()
	defer a.DebuggingSnapshotter.Flush()

	unschedulablePodLister := a.UnschedulablePodLister()
	scheduledPodLister := a.ScheduledPodLister()
	autoscalingContext := a.AutoscalingContext

	klog.V(4).Info("Starting main loop")

	stateUpdateStart := time.Now()

	// Get nodes and pods currently living on cluster
	allNodes, readyNodes, typedErr := a.obtainNodeLists(a.CloudProvider)
	if typedErr != nil {
		klog.Errorf("Failed to get node list: %v", typedErr)
		return typedErr
	}
	originalScheduledPods, err := scheduledPodLister.List()
	if err != nil {
		klog.Errorf("Failed to list scheduled pods: %v", err)
		return caerrors.ToAutoscalerError(caerrors.ApiCallError, err)
	}

	if abortLoop, err := a.processors.ActionableClusterProcessor.ShouldAbort(
		a.AutoscalingContext, allNodes, readyNodes, currentTime); abortLoop {
		return err
	}

	// Update cluster resource usage metrics
	coresTotal, memoryTotal := calculateCoresMemoryTotal(allNodes, currentTime)
	metrics.UpdateClusterCPUCurrentCores(coresTotal)
	metrics.UpdateClusterMemoryCurrentBytes(memoryTotal)

	daemonsets, err := a.ListerRegistry.DaemonSetLister().List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to get daemonset list: %v", err)
		return caerrors.ToAutoscalerError(caerrors.ApiCallError, err)
	}

	// Call CloudProvider.Refresh before any other calls to cloud provider.
	refreshStart := time.Now()
	err = a.AutoscalingContext.CloudProvider.Refresh()
	metrics.UpdateDurationFromStart(metrics.CloudProviderRefresh, refreshStart)
	if err != nil {
		klog.Errorf("Failed to refresh cloud provider config: %v", err)
		return caerrors.ToAutoscalerError(caerrors.CloudProviderError, err)
	}

	// Update node groups min/max and maximum number of nodes being set for all node groups after cloud provider refresh
	maxNodesCount := 0
	for _, nodeGroup := range a.AutoscalingContext.CloudProvider.NodeGroups() {
		metrics.UpdateNodeGroupMin(nodeGroup.Id(), nodeGroup.MinSize())
		metrics.UpdateNodeGroupMax(nodeGroup.Id(), nodeGroup.MaxSize())
		maxNodesCount += nodeGroup.MaxSize()
	}
	if a.MaxNodesTotal > 0 {
		metrics.UpdateMaxNodesCount(integer.IntMin(a.MaxNodesTotal, maxNodesCount))
	} else {
		metrics.UpdateMaxNodesCount(maxNodesCount)
	}
	nonExpendableScheduledPods := core_utils.FilterOutExpendablePods(originalScheduledPods, a.ExpendablePodsPriorityCutoff)
	// Initialize cluster state to ClusterSnapshot
	if typedErr := a.initializeClusterSnapshot(allNodes, nonExpendableScheduledPods); typedErr != nil {
		return typedErr.AddPrefix("failed to initialize ClusterSnapshot: ")
	}
	// Initialize Pod Disruption Budget tracking
	if typedErr := a.initializeRemainingPdbTracker(); typedErr != nil {
		return typedErr.AddPrefix("failed to initialize RemainingPdbTracker: ")
	}

	nodeInfosForGroups, autoscalerError := a.processors.TemplateNodeInfoProvider.Process(autoscalingContext, readyNodes, daemonsets, a.ignoredTaints, currentTime)
	if autoscalerError != nil {
		klog.Errorf("Failed to get node infos for groups: %v", autoscalerError)
		return autoscalerError.AddPrefix("failed to build node infos for node groups: ")
	}

	a.DebuggingSnapshotter.SetTemplateNodes(nodeInfosForGroups)

	nodeInfosForGroups, err = a.processors.NodeInfoProcessor.Process(autoscalingContext, nodeInfosForGroups)
	if err != nil {
		klog.Errorf("Failed to process nodeInfos: %v", err)
		return caerrors.ToAutoscalerError(caerrors.InternalError, err)
	}

	if typedErr := a.updateClusterState(allNodes, nodeInfosForGroups, currentTime); typedErr != nil {
		klog.Errorf("Failed to update cluster state: %v", typedErr)
		return typedErr
	}
	metrics.UpdateDurationFromStart(metrics.UpdateState, stateUpdateStart)

	scaleUpStatus := &status.ScaleUpStatus{Result: status.ScaleUpNotTried}
	scaleUpStatusProcessorAlreadyCalled := false
	scaleDownStatus := &scaledownstatus.ScaleDownStatus{Result: scaledownstatus.ScaleDownNotTried}
	scaleDownStatusProcessorAlreadyCalled := false

	defer func() {
		// Update status information when the loop is done (regardless of reason)
		if autoscalingContext.WriteStatusConfigMap {
			status := a.clusterStateRegistry.GetStatus(currentTime)
			utils.WriteStatusConfigMap(autoscalingContext.ClientSet, autoscalingContext.ConfigNamespace,
				status.GetReadableString(), a.AutoscalingContext.LogRecorder, a.AutoscalingContext.StatusConfigMapName)
		}

		// This deferred processor execution allows the processors to handle a situation when a scale-(up|down)
		// wasn't even attempted because e.g. the iteration exited earlier.
		if !scaleUpStatusProcessorAlreadyCalled && a.processors != nil && a.processors.ScaleUpStatusProcessor != nil {
			a.processors.ScaleUpStatusProcessor.Process(a.AutoscalingContext, scaleUpStatus)
		}
		if !scaleDownStatusProcessorAlreadyCalled && a.processors != nil && a.processors.ScaleDownStatusProcessor != nil {
			scaleDownStatus.SetUnremovableNodesInfo(a.scaleDownPlanner.UnremovableNodes(), a.scaleDownPlanner.NodeUtilizationMap(), a.CloudProvider)
			a.processors.ScaleDownStatusProcessor.Process(a.AutoscalingContext, scaleDownStatus)
		}

		err := a.processors.AutoscalingStatusProcessor.Process(a.AutoscalingContext, a.clusterStateRegistry, currentTime)
		if err != nil {
			klog.Errorf("AutoscalingStatusProcessor error: %v.", err)
		}
	}()

	// Check if there are any nodes that failed to register in Kubernetes
	// master.
	unregisteredNodes := a.clusterStateRegistry.GetUnregisteredNodes()
	if len(unregisteredNodes) > 0 {
		klog.V(1).Infof("%d unregistered nodes present", len(unregisteredNodes))
		removedAny, err := removeOldUnregisteredNodes(unregisteredNodes, autoscalingContext,
			a.clusterStateRegistry, currentTime, autoscalingContext.LogRecorder)
		// There was a problem with removing unregistered nodes. Retry in the next loop.
		if err != nil {
			klog.Warningf("Failed to remove unregistered nodes: %v", err)
		}
		if removedAny {
			klog.V(0).Infof("Some unregistered nodes were removed")
		}
	}

	if !a.clusterStateRegistry.IsClusterHealthy() {
		klog.Warning("Cluster is not ready for autoscaling")
		a.scaleDownPlanner.CleanUpUnneededNodes()
		autoscalingContext.LogRecorder.Eventf(apiv1.EventTypeWarning, "ClusterUnhealthy", "Cluster is unhealthy")
		return nil
	}

	danglingNodes, err := a.deleteCreatedNodesWithErrors()
	if err != nil {
		klog.Warningf("Failed to remove nodes that were created with errors, skipping iteration: %v", err)
		return nil
	}
	if danglingNodes {
		klog.V(0).Infof("Some nodes that failed to create were removed, skipping iteration")
		return nil
	}

	// Check if there has been a constant difference between the number of nodes in k8s and
	// the number of nodes on the cloud provider side.
	// TODO: andrewskim - add protection for ready AWS nodes.
	fixedSomething, err := fixNodeGroupSize(autoscalingContext, a.clusterStateRegistry, currentTime)
	if err != nil {
		klog.Errorf("Failed to fix node group sizes: %v", err)
		return caerrors.ToAutoscalerError(caerrors.CloudProviderError, err)
	}
	if fixedSomething {
		klog.V(0).Infof("Some node group target size was fixed, skipping the iteration")
		return nil
	}

	metrics.UpdateLastTime(metrics.Autoscaling, time.Now())

	unschedulablePods, err := unschedulablePodLister.List()
	if err != nil {
		klog.Errorf("Failed to list unscheduled pods: %v", err)
		return caerrors.ToAutoscalerError(caerrors.ApiCallError, err)
	}
	metrics.UpdateUnschedulablePodsCount(len(unschedulablePods))

	unschedulablePods = tpu.ClearTPURequests(unschedulablePods)

	// todo: move split and append below to separate PodListProcessor
	// Some unschedulable pods can be waiting for lower priority pods preemption so they have nominated node to run.
	// Such pods don't require scale up but should be considered during scale down.
	unschedulablePods, unschedulableWaitingForLowerPriorityPreemption := core_utils.FilterOutExpendableAndSplit(unschedulablePods, allNodes, a.ExpendablePodsPriorityCutoff)

	// modify the snapshot simulating scheduling of pods waiting for preemption.
	// this is not strictly correct as we are not simulating preemption itself but it matches
	// CA logic from before migration to scheduler framework. So let's keep it for now
	for _, p := range unschedulableWaitingForLowerPriorityPreemption {
		if err := a.ClusterSnapshot.AddPod(p, p.Status.NominatedNodeName); err != nil {
			klog.Errorf("Failed to update snapshot with pod %s waiting for preemption", err)
			return caerrors.ToAutoscalerError(caerrors.InternalError, err)
		}
	}

	// Upcoming nodes are recently created nodes that haven't registered in the cluster yet, or haven't become ready yet.
	upcomingCounts, registeredUpcoming := a.clusterStateRegistry.GetUpcomingNodes()
	// For each upcoming node we inject a placeholder node faked to appear ready into the cluster snapshot, so that we can pack unschedulable pods on
	// them and not trigger another scale-up.
	// The fake nodes are intentionally not added to the all nodes list, so that they are not considered as candidates for scale-down (which
	// doesn't make sense as they're not real).
	for _, upcomingNode := range getUpcomingNodeInfos(upcomingCounts, nodeInfosForGroups) {
		var pods []*apiv1.Pod
		for _, podInfo := range upcomingNode.Pods {
			pods = append(pods, podInfo.Pod)
		}
		err = a.ClusterSnapshot.AddNodeWithPods(upcomingNode.Node(), pods)
		if err != nil {
			klog.Errorf("Failed to add upcoming node %s to cluster snapshot: %v", upcomingNode.Node().Name, err)
			return caerrors.ToAutoscalerError(caerrors.InternalError, err)
		}
	}
	// Some upcoming nodes can already be registered in the cluster, but not yet ready - we still inject replacements for them above. The actual registered nodes
	// have to be filtered out of the all nodes list so that scale-down can't consider them as candidates. Otherwise, with aggressive scale-down settings, we
	// could be removing the nodes before they have a chance to first become ready (the duration of which should be unrelated to the scale-down settings).
	var allRegisteredUpcoming []string
	for _, ngRegisteredUpcoming := range registeredUpcoming {
		allRegisteredUpcoming = append(allRegisteredUpcoming, ngRegisteredUpcoming...)
	}
	allNodes = subtractNodesByName(allNodes, allRegisteredUpcoming)
	// Remove the nodes from the snapshot as well so that the state is consistent.
	for _, notStartedNodeName := range allRegisteredUpcoming {
		err := a.ClusterSnapshot.RemoveNode(notStartedNodeName)
		if err != nil {
			klog.Errorf("Failed to remove NotStarted node %s from cluster snapshot: %v", notStartedNodeName, err)
			// ErrNodeNotFound shouldn't happen (so it needs to be logged above if it does), but what we care about here is that the
			// node is not in the snapshot - so we don't have to error out in that case.
			if !errors.Is(err, clustersnapshot.ErrNodeNotFound) {
				return caerrors.ToAutoscalerError(caerrors.InternalError, err)
			}
		}
	}

	l, err := a.ClusterSnapshot.NodeInfos().List()
	if err != nil {
		klog.Errorf("Unable to fetch ClusterNode List for Debugging Snapshot, %v", err)
	} else {
		a.AutoscalingContext.DebuggingSnapshotter.SetClusterNodes(l)
	}

	unschedulablePodsToHelp, _ := a.processors.PodListProcessor.Process(a.AutoscalingContext, unschedulablePods)

	// finally, filter out pods that are too "young" to safely be considered for a scale-up (delay is configurable)
	unschedulablePodsToHelp = a.filterOutYoungPods(unschedulablePodsToHelp, currentTime)

	preScaleUp := func() time.Time {
		scaleUpStart := time.Now()
		metrics.UpdateLastTime(metrics.ScaleUp, scaleUpStart)
		return scaleUpStart
	}

	postScaleUp := func(scaleUpStart time.Time) (bool, caerrors.AutoscalerError) {
		metrics.UpdateDurationFromStart(metrics.ScaleUp, scaleUpStart)

		if a.processors != nil && a.processors.ScaleUpStatusProcessor != nil {
			a.processors.ScaleUpStatusProcessor.Process(autoscalingContext, scaleUpStatus)
			scaleUpStatusProcessorAlreadyCalled = true
		}

		if typedErr != nil {
			klog.Errorf("Failed to scale up: %v", typedErr)
			return true, typedErr
		}
		if scaleUpStatus.Result == status.ScaleUpSuccessful {
			a.lastScaleUpTime = currentTime
			// No scale down in this iteration.
			scaleDownStatus.Result = scaledownstatus.ScaleDownInCooldown
			return true, nil
		}
		return false, nil
	}

	if len(unschedulablePodsToHelp) == 0 {
		scaleUpStatus.Result = status.ScaleUpNotNeeded
		klog.V(1).Info("No unschedulable pods")
	} else if a.MaxNodesTotal > 0 && len(readyNodes) >= a.MaxNodesTotal {
		scaleUpStatus.Result = status.ScaleUpNoOptionsAvailable
		klog.V(1).Info("Max total nodes in cluster reached")
	} else if allPodsAreNew(unschedulablePodsToHelp, currentTime) {
		// The assumption here is that these pods have been created very recently and probably there
		// is more pods to come. In theory we could check the newest pod time but then if pod were created
		// slowly but at the pace of 1 every 2 seconds then no scale up would be triggered for long time.
		// We also want to skip a real scale down (just like if the pods were handled).
		a.processorCallbacks.DisableScaleDownForLoop()
		scaleUpStatus.Result = status.ScaleUpInCooldown
		klog.V(1).Info("Unschedulable pods are very new, waiting one iteration for more")
	} else {
		scaleUpStart := preScaleUp()
		scaleUpStatus, typedErr = a.scaleUpOrchestrator.ScaleUp(unschedulablePodsToHelp, readyNodes, daemonsets, nodeInfosForGroups)
		if exit, err := postScaleUp(scaleUpStart); exit {
			return err
		}
	}

	if a.ScaleDownEnabled {
		unneededStart := time.Now()

		klog.V(4).Infof("Calculating unneeded nodes")

		var scaleDownCandidates []*apiv1.Node
		var podDestinations []*apiv1.Node

		// podDestinations and scaleDownCandidates are initialized based on allNodes variable, which contains only
		// registered nodes in cluster.
		// It does not include any upcoming nodes which can be part of clusterSnapshot. As an alternative to using
		// allNodes here, we could use nodes from clusterSnapshot and explicitly filter out upcoming nodes here but it
		// is of little (if any) benefit.

		if a.processors == nil || a.processors.ScaleDownNodeProcessor == nil {
			scaleDownCandidates = allNodes
			podDestinations = allNodes
		} else {
			var err caerrors.AutoscalerError
			scaleDownCandidates, err = a.processors.ScaleDownNodeProcessor.GetScaleDownCandidates(
				autoscalingContext, allNodes)
			if err != nil {
				klog.Error(err)
				return err
			}
			podDestinations, err = a.processors.ScaleDownNodeProcessor.GetPodDestinationCandidates(autoscalingContext, allNodes)
			if err != nil {
				klog.Error(err)
				return err
			}
		}

		actuationStatus := a.scaleDownActuator.CheckStatus()
		typedErr := a.scaleDownPlanner.UpdateClusterState(podDestinations, scaleDownCandidates, actuationStatus, currentTime)
		// Update clusterStateRegistry and metrics regardless of whether ScaleDown was successful or not.
		unneededNodes := a.scaleDownPlanner.UnneededNodes()
		a.processors.ScaleDownCandidatesNotifier.Update(unneededNodes, currentTime)
		metrics.UpdateUnneededNodesCount(len(unneededNodes))
		if typedErr != nil {
			scaleDownStatus.Result = scaledownstatus.ScaleDownError
			klog.Errorf("Failed to scale down: %v", typedErr)
			return typedErr
		}

		metrics.UpdateDurationFromStart(metrics.FindUnneeded, unneededStart)

		scaleDownInCooldown := a.processorCallbacks.disableScaleDownForLoop ||
			a.lastScaleUpTime.Add(a.ScaleDownDelayAfterAdd).After(currentTime) ||
			a.lastScaleDownFailTime.Add(a.ScaleDownDelayAfterFailure).After(currentTime) ||
			a.lastScaleDownDeleteTime.Add(a.ScaleDownDelayAfterDelete).After(currentTime)

		klog.V(4).Infof("Scale down status: lastScaleUpTime=%s lastScaleDownDeleteTime=%v "+
			"lastScaleDownFailTime=%s scaleDownForbidden=%v scaleDownInCooldown=%v",
			a.lastScaleUpTime, a.lastScaleDownDeleteTime, a.lastScaleDownFailTime,
			a.processorCallbacks.disableScaleDownForLoop, scaleDownInCooldown)
		metrics.UpdateScaleDownInCooldown(scaleDownInCooldown)

		if scaleDownInCooldown {
			scaleDownStatus.Result = scaledownstatus.ScaleDownInCooldown
		} else {
			klog.V(4).Infof("Starting scale down")

			// We want to delete unneeded Node Groups only if there was no recent scale up,
			// and there is no current delete in progress and there was no recent errors.
			_, drained := actuationStatus.DeletionsInProgress()
			var removedNodeGroups []cloudprovider.NodeGroup
			if len(drained) == 0 {
				var err error
				removedNodeGroups, err = a.processors.NodeGroupManager.RemoveUnneededNodeGroups(autoscalingContext)
				if err != nil {
					klog.Errorf("Error while removing unneeded node groups: %v", err)
				}
			}

			scaleDownStart := time.Now()
			metrics.UpdateLastTime(metrics.ScaleDown, scaleDownStart)
			empty, needDrain := a.scaleDownPlanner.NodesToDelete(currentTime)
			scaleDownStatus, typedErr := a.scaleDownActuator.StartDeletion(empty, needDrain)
			a.scaleDownActuator.ClearResultsNotNewerThan(scaleDownStatus.NodeDeleteResultsAsOf)
			metrics.UpdateDurationFromStart(metrics.ScaleDown, scaleDownStart)
			metrics.UpdateUnremovableNodesCount(countsByReason(a.scaleDownPlanner.UnremovableNodes()))

			scaleDownStatus.RemovedNodeGroups = removedNodeGroups

			if scaleDownStatus.Result == scaledownstatus.ScaleDownNodeDeleteStarted {
				a.lastScaleDownDeleteTime = currentTime
				a.clusterStateRegistry.Recalculate()
			}

			if (scaleDownStatus.Result == scaledownstatus.ScaleDownNoNodeDeleted ||
				scaleDownStatus.Result == scaledownstatus.ScaleDownNoUnneeded) &&
				a.AutoscalingContext.AutoscalingOptions.MaxBulkSoftTaintCount != 0 {
				taintableNodes := a.scaleDownPlanner.UnneededNodes()
				untaintableNodes := subtractNodes(allNodes, taintableNodes)
				actuation.UpdateSoftDeletionTaints(a.AutoscalingContext, taintableNodes, untaintableNodes)
			}

			if a.processors != nil && a.processors.ScaleDownStatusProcessor != nil {
				scaleDownStatus.SetUnremovableNodesInfo(a.scaleDownPlanner.UnremovableNodes(), a.scaleDownPlanner.NodeUtilizationMap(), a.CloudProvider)
				a.processors.ScaleDownStatusProcessor.Process(autoscalingContext, scaleDownStatus)
				scaleDownStatusProcessorAlreadyCalled = true
			}

			if typedErr != nil {
				klog.Errorf("Failed to scale down: %v", typedErr)
				a.lastScaleDownFailTime = currentTime
				return typedErr
			}
		}
	}

	if a.EnforceNodeGroupMinSize {
		scaleUpStart := preScaleUp()
		scaleUpStatus, typedErr = a.scaleUpOrchestrator.ScaleUpToNodeGroupMinSize(readyNodes, nodeInfosForGroups)
		if exit, err := postScaleUp(scaleUpStart); exit {
			return err
		}
	}

	return nil
}

// Sets the target size of node groups to the current number of nodes in them
// if the difference was constant for a prolonged time. Returns true if managed
// to fix something.
func fixNodeGroupSize(context *context.AutoscalingContext, clusterStateRegistry *clusterstate.ClusterStateRegistry, currentTime time.Time) (bool, error) {
	fixed := false
	for _, nodeGroup := range context.CloudProvider.NodeGroups() {
		incorrectSize := clusterStateRegistry.GetIncorrectNodeGroupSize(nodeGroup.Id())
		if incorrectSize == nil {
			continue
		}
		if incorrectSize.FirstObserved.Add(context.MaxNodeProvisionTime).Before(currentTime) {
			delta := incorrectSize.CurrentSize - incorrectSize.ExpectedSize
			if delta < 0 {
				klog.V(0).Infof("Decreasing size of %s, expected=%d current=%d delta=%d", nodeGroup.Id(),
					incorrectSize.ExpectedSize,
					incorrectSize.CurrentSize,
					delta)
				if err := nodeGroup.DecreaseTargetSize(delta); err != nil {
					return fixed, fmt.Errorf("failed to decrease %s: %v", nodeGroup.Id(), err)
				}
				fixed = true
			}
		}
	}
	return fixed, nil
}

// Removes unregistered nodes if needed. Returns true if anything was removed and error if such occurred.
func removeOldUnregisteredNodes(unregisteredNodes []clusterstate.UnregisteredNode, context *context.AutoscalingContext,
	csr *clusterstate.ClusterStateRegistry, currentTime time.Time, logRecorder *utils.LogEventRecorder) (bool, error) {
	removedAny := false
	for _, unregisteredNode := range unregisteredNodes {
		if unregisteredNode.UnregisteredSince.Add(context.MaxNodeProvisionTime).Before(currentTime) {
			klog.V(0).Infof("Removing unregistered node %v", unregisteredNode.Node.Name)
			nodeGroup, err := context.CloudProvider.NodeGroupForNode(unregisteredNode.Node)
			if err != nil {
				klog.Warningf("Failed to get node group for %s: %v", unregisteredNode.Node.Name, err)
				return removedAny, err
			}
			if nodeGroup == nil || reflect.ValueOf(nodeGroup).IsNil() {
				klog.Warningf("No node group for node %s, skipping", unregisteredNode.Node.Name)
				continue
			}
			size, err := nodeGroup.TargetSize()
			if err != nil {
				klog.Warningf("Failed to get node group size; unregisteredNode=%v; nodeGroup=%v; err=%v", unregisteredNode.Node.Name, nodeGroup.Id(), err)
				continue
			}
			if nodeGroup.MinSize() >= size {
				klog.Warningf("Failed to remove node %s: node group min size reached, skipping unregistered node removal", unregisteredNode.Node.Name)
				continue
			}
			err = nodeGroup.DeleteNodes([]*apiv1.Node{unregisteredNode.Node})
			csr.InvalidateNodeInstancesCacheEntry(nodeGroup)
			if err != nil {
				klog.Warningf("Failed to remove node %s: %v", unregisteredNode.Node.Name, err)
				logRecorder.Eventf(apiv1.EventTypeWarning, "DeleteUnregisteredFailed",
					"Failed to remove node %s: %v", unregisteredNode.Node.Name, err)
				return removedAny, err
			}
			logRecorder.Eventf(apiv1.EventTypeNormal, "DeleteUnregistered",
				"Removed unregistered node %v", unregisteredNode.Node.Name)
			metrics.RegisterOldUnregisteredNodesRemoved(1)
			removedAny = true
		}
	}
	return removedAny, nil
}

func (a *StaticAutoscaler) deleteCreatedNodesWithErrors() (bool, error) {
	// We always schedule deleting of incoming errornous nodes
	// TODO[lukaszos] Consider adding logic to not retry delete every loop iteration
	nodes := a.clusterStateRegistry.GetCreatedNodesWithErrors()

	nodeGroups := a.nodeGroupsById()
	nodesToBeDeletedByNodeGroupId := make(map[string][]*apiv1.Node)

	for _, node := range nodes {
		nodeGroup, err := a.CloudProvider.NodeGroupForNode(node)
		if err != nil {
			id := "<nil>"
			if node != nil {
				id = node.Spec.ProviderID
			}
			klog.Warningf("Cannot determine nodeGroup for node %v; %v", id, err)
			continue
		}
		if nodeGroup == nil || reflect.ValueOf(nodeGroup).IsNil() {
			a.clusterStateRegistry.RefreshCloudProviderNodeInstancesCache()
			return false, fmt.Errorf("node %s has no known nodegroup", node.GetName())
		}
		nodesToBeDeletedByNodeGroupId[nodeGroup.Id()] = append(nodesToBeDeletedByNodeGroupId[nodeGroup.Id()], node)
	}

	deletedAny := false

	for nodeGroupId, nodesToBeDeleted := range nodesToBeDeletedByNodeGroupId {
		var err error
		klog.V(1).Infof("Deleting %v from %v node group because of create errors", len(nodesToBeDeleted), nodeGroupId)

		nodeGroup := nodeGroups[nodeGroupId]
		if nodeGroup == nil {
			err = fmt.Errorf("node group %s not found", nodeGroupId)
		} else {
			err = nodeGroup.DeleteNodes(nodesToBeDeleted)
		}

		if err != nil {
			klog.Warningf("Error while trying to delete nodes from %v: %v", nodeGroupId, err)
		}

		deletedAny = deletedAny || err == nil
		a.clusterStateRegistry.InvalidateNodeInstancesCacheEntry(nodeGroup)
	}

	return deletedAny, nil
}

func (a *StaticAutoscaler) nodeGroupsById() map[string]cloudprovider.NodeGroup {
	nodeGroups := make(map[string]cloudprovider.NodeGroup)
	for _, nodeGroup := range a.CloudProvider.NodeGroups() {
		nodeGroups[nodeGroup.Id()] = nodeGroup
	}
	return nodeGroups
}

// Don't consider pods newer than newPodScaleUpDelay or annotated podScaleUpDelay
// seconds old as unschedulable.
func (a *StaticAutoscaler) filterOutYoungPods(allUnschedulablePods []*apiv1.Pod, currentTime time.Time) []*apiv1.Pod {
	var oldUnschedulablePods []*apiv1.Pod
	newPodScaleUpDelay := a.AutoscalingOptions.NewPodScaleUpDelay
	for _, pod := range allUnschedulablePods {
		podAge := currentTime.Sub(pod.CreationTimestamp.Time)
		podScaleUpDelay := newPodScaleUpDelay

		if podScaleUpDelayAnnotationStr, ok := pod.Annotations[podScaleUpDelayAnnotationKey]; ok {
			podScaleUpDelayAnnotation, err := time.ParseDuration(podScaleUpDelayAnnotationStr)
			if err != nil {
				klog.Errorf("Failed to parse pod %q annotation %s: %v", pod.Name, podScaleUpDelayAnnotationKey, err)
			} else {
				if podScaleUpDelayAnnotation < podScaleUpDelay {
					klog.Errorf("Failed to set pod scale up delay for %q through annotation %s: %d is less then %d", pod.Name, podScaleUpDelayAnnotationKey, podScaleUpDelayAnnotation, newPodScaleUpDelay)
				} else {
					podScaleUpDelay = podScaleUpDelayAnnotation
				}
			}
		}

		if podAge > podScaleUpDelay {
			oldUnschedulablePods = append(oldUnschedulablePods, pod)
		} else {
			klog.V(3).Infof("Pod %s is %.3f seconds old, too new to consider unschedulable", pod.Name, podAge.Seconds())
		}
	}
	return oldUnschedulablePods
}

// ExitCleanUp performs all necessary clean-ups when the autoscaler's exiting.
func (a *StaticAutoscaler) ExitCleanUp() {
	a.processors.CleanUp()
	a.DebuggingSnapshotter.Cleanup()

	if !a.AutoscalingContext.WriteStatusConfigMap {
		return
	}
	utils.DeleteStatusConfigMap(a.AutoscalingContext.ClientSet, a.AutoscalingContext.ConfigNamespace, a.AutoscalingContext.StatusConfigMapName)

	a.clusterStateRegistry.Stop()
}

func (a *StaticAutoscaler) obtainNodeLists(cp cloudprovider.CloudProvider) ([]*apiv1.Node, []*apiv1.Node, caerrors.AutoscalerError) {
	allNodes, err := a.AllNodeLister().List()
	if err != nil {
		klog.Errorf("Failed to list all nodes: %v", err)
		return nil, nil, caerrors.ToAutoscalerError(caerrors.ApiCallError, err)
	}
	readyNodes, err := a.ReadyNodeLister().List()
	if err != nil {
		klog.Errorf("Failed to list ready nodes: %v", err)
		return nil, nil, caerrors.ToAutoscalerError(caerrors.ApiCallError, err)
	}

	// Handle GPU case - allocatable GPU may be equal to 0 up to 15 minutes after
	// node registers as ready. See https://github.com/kubernetes/kubernetes/issues/54959
	// Treat those nodes as unready until GPU actually becomes available and let
	// our normal handling for booting up nodes deal with this.
	// TODO: Remove this call when we handle dynamically provisioned resources.
	allNodes, readyNodes = a.processors.CustomResourcesProcessor.FilterOutNodesWithUnreadyResources(a.AutoscalingContext, allNodes, readyNodes)
	allNodes, readyNodes = taints.FilterOutNodesWithIgnoredTaints(a.ignoredTaints, allNodes, readyNodes)
	return allNodes, readyNodes, nil
}

func (a *StaticAutoscaler) updateClusterState(allNodes []*apiv1.Node, nodeInfosForGroups map[string]*schedulerframework.NodeInfo, currentTime time.Time) caerrors.AutoscalerError {
	err := a.clusterStateRegistry.UpdateNodes(allNodes, nodeInfosForGroups, currentTime)
	if err != nil {
		klog.Errorf("Failed to update node registry: %v", err)
		a.scaleDownPlanner.CleanUpUnneededNodes()
		return caerrors.ToAutoscalerError(caerrors.CloudProviderError, err)
	}
	core_utils.UpdateClusterStateMetrics(a.clusterStateRegistry)

	return nil
}

func allPodsAreNew(pods []*apiv1.Pod, currentTime time.Time) bool {
	if core_utils.GetOldestCreateTime(pods).Add(unschedulablePodTimeBuffer).After(currentTime) {
		return true
	}
	found, oldest := core_utils.GetOldestCreateTimeWithGpu(pods)
	return found && oldest.Add(unschedulablePodWithGpuTimeBuffer).After(currentTime)
}

func getUpcomingNodeInfos(upcomingCounts map[string]int, nodeInfos map[string]*schedulerframework.NodeInfo) []*schedulerframework.NodeInfo {
	upcomingNodes := make([]*schedulerframework.NodeInfo, 0)
	for nodeGroup, numberOfNodes := range upcomingCounts {
		nodeTemplate, found := nodeInfos[nodeGroup]
		if !found {
			klog.Warningf("Couldn't find template for node group %s", nodeGroup)
			continue
		}

		if nodeTemplate.Node().Annotations == nil {
			nodeTemplate.Node().Annotations = make(map[string]string)
		}
		nodeTemplate.Node().Annotations[NodeUpcomingAnnotation] = "true"

		for i := 0; i < numberOfNodes; i++ {
			// Ensure new nodes have different names because nodeName
			// will be used as a map key. Also deep copy pods (daemonsets &
			// any pods added by cloud provider on template).
			upcomingNodes = append(upcomingNodes, scheduler_utils.DeepCopyTemplateNode(nodeTemplate, fmt.Sprintf("upcoming-%d", i)))
		}
	}
	return upcomingNodes
}

func calculateCoresMemoryTotal(nodes []*apiv1.Node, timestamp time.Time) (int64, int64) {
	// this function is essentially similar to the calculateScaleDownCoresMemoryTotal
	// we want to check all nodes, aside from those deleting, to sum the cluster resource usage.
	var coresTotal, memoryTotal int64
	for _, node := range nodes {
		if actuation.IsNodeBeingDeleted(node, timestamp) {
			// Nodes being deleted do not count towards total cluster resources
			continue
		}
		cores, memory := core_utils.GetNodeCoresAndMemory(node)

		coresTotal += cores
		memoryTotal += memory
	}

	return coresTotal, memoryTotal
}

func countsByReason(nodes []*simulator.UnremovableNode) map[simulator.UnremovableReason]int {
	counts := make(map[simulator.UnremovableReason]int)

	for _, node := range nodes {
		counts[node.Reason]++
	}

	return counts
}

func subtractNodesByName(nodes []*apiv1.Node, namesToRemove []string) []*apiv1.Node {
	var c []*apiv1.Node
	removeSet := make(map[string]bool)
	for _, name := range namesToRemove {
		removeSet[name] = true
	}
	for _, n := range nodes {
		if removeSet[n.Name] {
			continue
		}
		c = append(c, n)
	}
	return c
}

func subtractNodes(a []*apiv1.Node, b []*apiv1.Node) []*apiv1.Node {
	return subtractNodesByName(a, nodeNames(b))
}

func nodeNames(ns []*apiv1.Node) []string {
	names := make([]string, len(ns))
	for i, node := range ns {
		names[i] = node.Name
	}
	return names
}
