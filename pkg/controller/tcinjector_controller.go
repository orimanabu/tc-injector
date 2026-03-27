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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	tcv1alpha1 "github.com/tc-injector/tc-injector/pkg/api/v1alpha1"
	"github.com/tc-injector/tc-injector/pkg/tc"
)

// VethFinder resolves a container ID to the host-side veth interface name.
type VethFinder interface {
	FindHostVeth(ctx context.Context, containerID string) (string, error)
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

// Reconciler reconciles TCInjector objects and manages tc rules on the local node.
type Reconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	NodeName  string
	Finder    VethFinder
	TCApplier TCApplier

	// mu guards injected.
	mu sync.Mutex
	// injected tracks which pods have tc rules applied: podUID -> ifaceName.
	injected map[string]string
}

// +kubebuilder:rbac:groups=tc-injector.example.com,resources=tcinjectors,verbs=get;list;watch
// +kubebuilder:rbac:groups=tc-injector.example.com,resources=tcinjectors/status,verbs=update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

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

	// Build the desired state: podUID -> delayMs.
	desired := make(map[string]int32)
	for _, injector := range injectorList.Items {
		if injector.DeletionTimestamp != nil {
			continue
		}
		for _, rule := range injector.Spec.Rules {
			sel, err := metav1.LabelSelectorAsSelector(&rule.Selector)
			if err != nil {
				logger.Error(err, "invalid label selector", "injector", injector.Name)
				continue
			}
			for _, pod := range podList.Items {
				if !isPodReady(&pod) {
					continue
				}
				if sel.Matches(labels.Set(pod.Labels)) {
					delay := tc.RandomDelay(rule.MinDelay, rule.MaxDelay)
					// Last matching rule wins; earlier rules can be overridden.
					desired[string(pod.UID)] = delay
				}
			}
		}
	}

	// Reconcile actual vs desired.
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.injected == nil {
		r.injected = make(map[string]string)
	}

	// Remove tc rules for pods that are no longer desired.
	for uid, iface := range r.injected {
		if _, ok := desired[uid]; !ok {
			logger.Info("removing tc rule", "podUID", uid, "iface", iface)
			if err := r.TCApplier.Remove(iface); err != nil {
				logger.Error(err, "failed to remove tc rule", "iface", iface)
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

		iface, err := r.Finder.FindHostVeth(ctx, containerID)
		if err != nil {
			logger.Error(err, "cannot find host veth", "pod", pod.Name, "containerID", containerID)
			continue
		}

		tcCmd := fmt.Sprintf("tc qdisc replace dev %s root handle 1: netem delay %dms", iface, delayMs)
		logger.Info("applying tc delay", "pod", pod.Name, "iface", iface, "delayMs", delayMs, "tcCmd", tcCmd)
		if err := r.TCApplier.Apply(iface, delayMs); err != nil {
			logger.Error(err, "tc apply failed", "iface", iface)
			continue
		}

		r.injected[uid] = iface
		injectedCount++
	}

	// Update status on the triggering TCInjector if the request was for one.
	if req.Name != "" {
		injector := &tcv1alpha1.TCInjector{}
		if err := r.Get(ctx, req.NamespacedName, injector); err == nil {
			injector.Status.InjectedPods = injectedCount
			setCondition(injector, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "Reconciled",
				Message:            fmt.Sprintf("%d pod(s) injected on node %s", injectedCount, r.NodeName),
				LastTransitionTime: metav1.Now(),
			})
			if err := r.Status().Update(ctx, injector); err != nil && !errors.IsConflict(err) {
				logger.Error(err, "failed to update TCInjector status")
			}
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
		For(&tcv1alpha1.TCInjector{}).
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
