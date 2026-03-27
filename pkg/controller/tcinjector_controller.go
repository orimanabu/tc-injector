// Package controller implements the TCInjector reconciliation loop.
package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	tcv1alpha1 "github.com/tc-injector/tc-injector/pkg/api/v1alpha1"
	"github.com/tc-injector/tc-injector/pkg/tc"
)

// VethFinder resolves a container ID to the host-side veth interface name and ifindex.
type VethFinder interface {
	FindHostVeth(ctx context.Context, containerID string) (ifaceName string, ifaceIndex int, err error)
}

// TCApplier applies and removes tc netem delay rules on network interfaces.
type TCApplier interface {
	Apply(iface string, delayMs int32) error
	Remove(iface string) error
}

// RealTCApplier delegates to the tc package and is used in production.
type RealTCApplier struct{}

func (RealTCApplier) Apply(iface string, delayMs int32) error { return tc.Apply(iface, delayMs) }
func (RealTCApplier) Remove(iface string) error               { return tc.Remove(iface) }

// injectedState records the tc rule currently applied to a pod.
type injectedState struct {
	ifaceName    string
	ifaceIndex   int
	podName      string
	podNamespace string
	delayMs      int32
	tcCmd        string
}

// Reconciler reconciles TCInjector objects and manages tc rules on the local node.
type Reconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	NodeName  string
	Finder    VethFinder
	TCApplier TCApplier

	// mu guards injected.
	mu sync.Mutex
	// injected tracks which pods have tc rules applied: podUID -> injectedState.
	injected map[string]injectedState
}

// +kubebuilder:rbac:groups=tc-injector.setns.net,resources=tcinjectors,verbs=get;list;watch
// +kubebuilder:rbac:groups=tc-injector.setns.net,resources=tcinjectors/status,verbs=update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile is called when a TCInjector or Pod changes.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("request", req)

	// Fetch all TCInjector resources.
	injectorList := &tcv1alpha1.TCInjectorList{}
	if err := r.List(ctx, injectorList); err != nil {
		return reconcile.Result{}, fmt.Errorf("list TCInjectors: %w", err)
	}

	// Fetch all pods on this node.
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.MatchingFields{"spec.nodeName": r.NodeName}); err != nil {
		return reconcile.Result{}, fmt.Errorf("list pods on node %s: %w", r.NodeName, err)
	}

	// Fetch all namespaces and build a map of name -> labels for selector matching.
	namespaceList := &corev1.NamespaceList{}
	if err := r.List(ctx, namespaceList); err != nil {
		return reconcile.Result{}, fmt.Errorf("list namespaces: %w", err)
	}
	namespaceLabelMap := make(map[string]labels.Set, len(namespaceList.Items))
	for _, ns := range namespaceList.Items {
		namespaceLabelMap[ns.Name] = labels.Set(ns.Labels)
	}

	// Reconcile actual vs desired under the lock.
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.injected == nil {
		r.injected = make(map[string]injectedState)
	}

	// Build the desired state: podUID -> delayMs.
	// For pods already injected without periodic rotation, the existing delay is
	// preserved to avoid re-randomizing on every reconcile.
	desired := make(map[string]int32)
	for _, injector := range injectorList.Items {
		if injector.DeletionTimestamp != nil {
			continue
		}
		shouldRotate := injector.Spec.EnablePeriodicDelayRotation
		for _, rule := range injector.Spec.Rules {
			podSel, err := metav1.LabelSelectorAsSelector(&rule.Selector)
			if err != nil {
				logger.Error(err, "invalid label selector", "injector", injector.Name)
				continue
			}
			nsSel, err := metav1.LabelSelectorAsSelector(&rule.NamespaceSelector)
			if err != nil {
				logger.Error(err, "invalid namespace selector", "injector", injector.Name)
				continue
			}
			for _, pod := range podList.Items {
				if !isPodReady(&pod) {
					continue
				}
				if !podSel.Matches(labels.Set(pod.Labels)) {
					continue
				}
				// Use empty labels if namespace is not found (e.g., recently deleted).
				// An empty NamespaceSelector matches all namespaces including this case.
				nsLabels := namespaceLabelMap[pod.Namespace]
				if !nsSel.Matches(nsLabels) {
					continue
				}
				uid := string(pod.UID)
				// Preserve the existing delay when the pod is already injected and
				// periodic rotation is not active. This prevents re-randomizing on
				// every reconcile triggered by unrelated pod or resource changes.
				// Last matching rule wins; earlier rules can be overridden.
				if existing, alreadyInjected := r.injected[uid]; alreadyInjected && !shouldRotate {
					desired[uid] = existing.delayMs
				} else {
					desired[uid] = tc.RandomDelay(rule.MinDelay, rule.MaxDelay)
				}
			}
		}
	}

	// Remove tc rules for pods that are no longer desired.
	for uid, state := range r.injected {
		if _, ok := desired[uid]; !ok {
			logger.Info("removing tc rule", "podUID", uid, "iface", state.ifaceName)
			if err := r.TCApplier.Remove(state.ifaceName); err != nil {
				logger.Error(err, "failed to remove tc rule", "iface", state.ifaceName)
			}
			delete(r.injected, uid)
		}
	}

	// Apply tc rules for pods in the desired set.
	var injectedCount int32
	for uid, delayMs := range desired {
		pod := findPodByUID(podList.Items, uid)
		if pod == nil {
			continue
		}

		containerID := firstReadyContainerID(pod)
		if containerID == "" {
			logger.Info("pod has no ready container yet, skipping", "pod", pod.Name)
			continue
		}

		iface, ifaceIdx, err := r.Finder.FindHostVeth(ctx, containerID)
		if err != nil {
			logger.Error(err, "cannot find host veth", "pod", pod.Name, "containerID", containerID)
			continue
		}

		// Skip if the same interface and delay are already applied.
		if existing, ok := r.injected[uid]; ok && existing.ifaceName == iface && existing.delayMs == delayMs {
			injectedCount++
			continue
		}

		tcCmd := fmt.Sprintf("tc qdisc replace dev %s root handle 1: netem delay %dms", iface, delayMs)
		logger.Info("applying tc delay", "pod", pod.Name, "iface", iface, "delayMs", delayMs, "tcCmd", tcCmd)
		if err := r.TCApplier.Apply(iface, delayMs); err != nil {
			logger.Error(err, "tc apply failed", "iface", iface)
			continue
		}

		r.injected[uid] = injectedState{
			ifaceName:    iface,
			ifaceIndex:   ifaceIdx,
			podName:      pod.Name,
			podNamespace: pod.Namespace,
			delayMs:      delayMs,
			tcCmd:        tcCmd,
		}
		injectedCount++
	}

	// Update status on the triggering TCInjector if the request was for one.
	// Retry on conflict: multiple DaemonSet pods (one per node) may update the
	// same TCInjector status concurrently. On conflict we re-read the latest
	// resource version and re-merge before retrying.
	if req.Name != "" {
		// Build this node's details once; they do not change between retries.
		thisNodeDetails := make([]tcv1alpha1.InjectedPodStatus, 0, len(r.injected))
		for _, state := range r.injected {
			thisNodeDetails = append(thisNodeDetails, tcv1alpha1.InjectedPodStatus{
				NodeName:       r.NodeName,
				Namespace:      state.podNamespace,
				PodName:        state.podName,
				Interface:      state.ifaceName,
				InterfaceIndex: int32(state.ifaceIndex),
				DelayMs:        state.delayMs,
				TCCommand:      state.tcCmd,
			})
		}
		for attempt := 0; attempt < 5; attempt++ {
			injector := &tcv1alpha1.TCInjector{}
			if err := r.Get(ctx, req.NamespacedName, injector); err != nil {
				logger.Error(err, "failed to get TCInjector for status update")
				break
			}
			// Preserve entries from other nodes; replace only this node's entries.
			merged := make([]tcv1alpha1.InjectedPodStatus, 0, len(injector.Status.InjectedPodDetails))
			for _, d := range injector.Status.InjectedPodDetails {
				if d.NodeName != r.NodeName {
					merged = append(merged, d)
				}
			}
			merged = append(merged, thisNodeDetails...)
			injector.Status.InjectedPods = int32(len(merged))
			injector.Status.InjectedPodDetails = merged
			// Use the node name as the condition type so each node upserts its own condition.
			setCondition(injector, metav1.Condition{
				Type:               r.NodeName,
				Status:             metav1.ConditionTrue,
				Reason:             "Reconciled",
				Message:            fmt.Sprintf("%d pod(s) injected on node %s", injectedCount, r.NodeName),
				LastTransitionTime: metav1.Now(),
			})
			if err := r.Status().Update(ctx, injector); err != nil {
				if errors.IsConflict(err) {
					logger.V(1).Info("status update conflict, retrying", "node", r.NodeName, "attempt", attempt+1)
					continue
				}
				logger.Error(err, "failed to update TCInjector status")
			}
			break
		}
	}

	// If any TCInjector has periodic delay rotation enabled, requeue after the shortest interval.
	var requeueAfter time.Duration
	for _, injector := range injectorList.Items {
		if injector.DeletionTimestamp != nil || !injector.Spec.EnablePeriodicDelayRotation {
			continue
		}
		interval := 30 * time.Second
		if injector.Spec.DelayInterval != nil && injector.Spec.DelayInterval.Duration > 0 {
			interval = injector.Spec.DelayInterval.Duration
		}
		if requeueAfter == 0 || interval < requeueAfter {
			requeueAfter = interval
		}
	}
	return reconcile.Result{RequeueAfter: requeueAfter}, nil
}

// SetupWithManager registers the controller with the manager, watching both
// TCInjector resources and Pods on this node.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Index pods by spec.nodeName for efficient listing.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&corev1.Pod{},
		"spec.nodeName",
		func(obj client.Object) []string {
			pod := obj.(*corev1.Pod)
			return []string{pod.Spec.NodeName}
		},
	); err != nil {
		return fmt.Errorf("add nodeName field index: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		// GenerationChangedPredicate prevents reconciles triggered by status
		// updates, which do not increment metadata.generation. Without this,
		// each Status().Update() call would re-trigger reconcile indefinitely.
		For(&tcv1alpha1.TCInjector{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Also trigger reconcile when pods on this node change.
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.podToTCInjector),
		).
		Complete(r)
}

// podToTCInjector maps a Pod event to reconcile requests for all TCInjectors.
func (r *Reconciler) podToTCInjector(ctx context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Spec.NodeName != r.NodeName {
		return nil
	}

	injectorList := &tcv1alpha1.TCInjectorList{}
	if err := r.Client.List(ctx, injectorList); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(injectorList.Items))
	for _, injector := range injectorList.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{Name: injector.Name, Namespace: injector.Namespace},
		})
	}
	return requests
}

// isPodReady returns true if the pod is running and all containers are ready.
func isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return true
}

// firstReadyContainerID returns the container ID of the first ready container.
func firstReadyContainerID(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Ready && cs.ContainerID != "" {
			return cs.ContainerID
		}
	}
	return ""
}

// findPodByUID returns the pod with the given UID from the list.
func findPodByUID(pods []corev1.Pod, uid string) *corev1.Pod {
	for i := range pods {
		if string(pods[i].UID) == uid {
			return &pods[i]
		}
	}
	return nil
}

// setCondition upserts a condition on the TCInjector status.
func setCondition(injector *tcv1alpha1.TCInjector, cond metav1.Condition) {
	for i, existing := range injector.Status.Conditions {
		if existing.Type == cond.Type {
			injector.Status.Conditions[i] = cond
			return
		}
	}
	injector.Status.Conditions = append(injector.Status.Conditions, cond)
}
