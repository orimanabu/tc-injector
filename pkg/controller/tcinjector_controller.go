// Package controller implements the TCInjector reconciliation loop.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
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

// multusNetworkStatusAnnotation is the pod annotation written by Multus that lists
// all attached network interfaces and their NetworkAttachmentDefinition names.
// The authoritative key is defined in the network-attachment-definition-client library
// as NetworkStatusAnnot = "k8s.v1.cni.cncf.io/network-status" (no trailing 's' on "network").
const multusNetworkStatusAnnotation = "k8s.v1.cni.cncf.io/network-status"

// VethFinder resolves a container ID to the host-side veth interface name/ifindex
// and to the pod's network namespace path.
type VethFinder interface {
	FindHostVeth(ctx context.Context, containerID string) (ifaceName string, ifaceIndex int, err error)
	FindNetnsPath(ctx context.Context, containerID string) (string, error)
}

// TCApplier applies and removes tc netem delay rules on network interfaces.
type TCApplier interface {
	Apply(iface string, delayMs int32) error
	Remove(iface string) error
	// ApplyInNetns installs or replaces a netem qdisc on iface inside the network
	// namespace at netnsPath. Returns the tc command string that was executed.
	ApplyInNetns(netnsPath, iface string, delayMs int32) (string, error)
	// RemoveInNetns removes the root qdisc from iface inside the given network
	// namespace. It is a no-op when the namespace or interface no longer exists.
	RemoveInNetns(netnsPath, iface string) error
}

// RealTCApplier delegates to the tc package and is used in production.
type RealTCApplier struct{}

func (RealTCApplier) Apply(iface string, delayMs int32) error { return tc.Apply(iface, delayMs) }
func (RealTCApplier) Remove(iface string) error               { return tc.Remove(iface) }
func (RealTCApplier) ApplyInNetns(netnsPath, iface string, delayMs int32) (string, error) {
	return tc.ApplyInNetns(netnsPath, iface, delayMs)
}
func (RealTCApplier) RemoveInNetns(netnsPath, iface string) error {
	return tc.RemoveInNetns(netnsPath, iface)
}

// multusInjectedState records the tc rule applied to a single Multus-managed interface.
type multusInjectedState struct {
	nadName   string // NetworkAttachmentDefinition identifier from the annotation
	ifaceName string // interface name inside the pod (e.g. net1)
	netnsPath string // /proc/<pid>/ns/net used to enter the pod network namespace
	delayMs   int32
	tcCmd     string
}

// injectedState records all tc rules currently applied to a pod.
type injectedState struct {
	ifaceName        string // host-side veth interface name
	ifaceIndex       int
	podName          string
	podNamespace     string
	delayMs          int32
	tcCmd            string
	multusInterfaces []multusInjectedState
}

// desiredPodState holds the computed tc configuration that should be applied to a pod.
type desiredPodState struct {
	delayMs          int32
	multusInterfaces []multusDesiredIface
}

// multusDesiredIface identifies a Multus-managed interface to inject delay into.
type multusDesiredIface struct {
	nadName   string // as it appears in the annotation (e.g. "default/mynetwork")
	ifaceName string // interface name inside the pod (e.g. net1)
}

// multusNetworkStatus corresponds to one entry in the Multus networks-status annotation.
type multusNetworkStatus struct {
	Name      string `json:"name"`
	Interface string `json:"interface"`
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

	// Build the desired state: podUID -> desiredPodState.
	// For pods already injected without periodic rotation, the existing delay is
	// preserved to avoid re-randomizing on every reconcile.
	desired := make(map[string]desiredPodState)
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
				var delayMs int32
				if existing, alreadyInjected := r.injected[uid]; alreadyInjected && !shouldRotate {
					delayMs = existing.delayMs
				} else {
					delayMs = tc.RandomDelay(rule.MinDelay, rule.MaxDelay)
				}
				multusIfaces := resolveMultusInterfaces(logger, &pod, rule.MultusNetworks)
				if len(rule.MultusNetworks) > 0 && len(multusIfaces) == 0 {
					logger.Info("no multus interfaces resolved for pod; check annotation and NAD names",
						"pod", pod.Name, "namespace", pod.Namespace,
						"multusNetworks", rule.MultusNetworks)
				}
				desired[uid] = desiredPodState{
					delayMs:          delayMs,
					multusInterfaces: multusIfaces,
				}
			}
		}
	}

	// Remove tc rules for pods that are no longer in the desired set.
	for uid, state := range r.injected {
		if _, ok := desired[uid]; !ok {
			logger.Info("removing tc rules", "podUID", uid, "iface", state.ifaceName)
			if err := r.TCApplier.Remove(state.ifaceName); err != nil {
				logger.Error(err, "failed to remove tc rule", "iface", state.ifaceName)
			}
			for _, mi := range state.multusInterfaces {
				if err := r.TCApplier.RemoveInNetns(mi.netnsPath, mi.ifaceName); err != nil {
					logger.Error(err, "failed to remove multus tc rule",
						"iface", mi.ifaceName, "nad", mi.nadName)
				}
			}
			delete(r.injected, uid)
		}
	}

	// Apply tc rules for pods in the desired set.
	var injectedCount int32
	for uid, des := range desired {
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

		// Resolve netns path only when Multus interfaces are requested.
		var netnsPath string
		if len(des.multusInterfaces) > 0 {
			netnsPath, err = r.Finder.FindNetnsPath(ctx, containerID)
			if err != nil {
				logger.Error(err, "cannot find netns path for multus injection", "pod", pod.Name)
				// Continue: the primary interface can still be processed.
			}
		}

		existing := r.injected[uid]

		// Check whether primary interface or Multus interfaces changed.
		primaryUnchanged := existing.ifaceName == iface && existing.delayMs == des.delayMs
		multusUnchanged := multusIfaceSetsEqual(existing.multusInterfaces, des.multusInterfaces, des.delayMs)
		if primaryUnchanged && multusUnchanged {
			injectedCount++
			continue
		}

		// Apply primary interface if changed.
		var newTCCmd string
		if !primaryUnchanged {
			newTCCmd = fmt.Sprintf("tc qdisc replace dev %s root handle 1: netem delay %dms", iface, des.delayMs)
			logger.Info("applying tc delay", "pod", pod.Name, "iface", iface,
				"delayMs", des.delayMs, "tcCmd", newTCCmd)
			if err := r.TCApplier.Apply(iface, des.delayMs); err != nil {
				logger.Error(err, "tc apply failed", "iface", iface)
				continue
			}
		} else {
			newTCCmd = existing.tcCmd
		}

		// Build a map of existing Multus state for efficient lookup.
		existingMultusMap := make(map[string]multusInjectedState, len(existing.multusInterfaces))
		for _, mi := range existing.multusInterfaces {
			existingMultusMap[mi.nadName] = mi
		}

		// Remove Multus interfaces that are no longer desired (NAD removed from rule).
		desiredNADs := make(map[string]bool, len(des.multusInterfaces))
		for _, dmi := range des.multusInterfaces {
			desiredNADs[dmi.nadName] = true
		}
		for _, mi := range existing.multusInterfaces {
			if !desiredNADs[mi.nadName] {
				if err := r.TCApplier.RemoveInNetns(mi.netnsPath, mi.ifaceName); err != nil {
					logger.Error(err, "failed to remove multus tc rule",
						"iface", mi.ifaceName, "nad", mi.nadName)
				}
			}
		}

		// Apply desired Multus interfaces (add new or update changed ones).
		newMultus := make([]multusInjectedState, 0, len(des.multusInterfaces))
		for _, dmi := range des.multusInterfaces {
			emi, wasInjected := existingMultusMap[dmi.nadName]
			miUnchanged := wasInjected &&
				emi.ifaceName == dmi.ifaceName &&
				emi.delayMs == des.delayMs

			if miUnchanged {
				newMultus = append(newMultus, emi)
				continue
			}

			if netnsPath == "" {
				logger.Info("skipping multus interface: netns path unavailable",
					"pod", pod.Name, "nad", dmi.nadName)
				continue
			}

			miCmd, err := r.TCApplier.ApplyInNetns(netnsPath, dmi.ifaceName, des.delayMs)
			if err != nil {
				logger.Error(err, "multus tc apply failed",
					"pod", pod.Name, "iface", dmi.ifaceName, "nad", dmi.nadName)
				continue
			}
			logger.Info("applying multus tc delay", "pod", pod.Name,
				"nad", dmi.nadName, "iface", dmi.ifaceName, "delayMs", des.delayMs,
				"tcCmd", miCmd)
			newMultus = append(newMultus, multusInjectedState{
				nadName:   dmi.nadName,
				ifaceName: dmi.ifaceName,
				netnsPath: netnsPath,
				delayMs:   des.delayMs,
				tcCmd:     miCmd,
			})
		}

		r.injected[uid] = injectedState{
			ifaceName:        iface,
			ifaceIndex:       ifaceIdx,
			podName:          pod.Name,
			podNamespace:     pod.Namespace,
			delayMs:          des.delayMs,
			tcCmd:            newTCCmd,
			multusInterfaces: newMultus,
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
			podStatus := tcv1alpha1.InjectedPodStatus{
				NodeName:       r.NodeName,
				Namespace:      state.podNamespace,
				PodName:        state.podName,
				Interface:      state.ifaceName,
				InterfaceIndex: int32(state.ifaceIndex),
				DelayMs:        state.delayMs,
				TCCommand:      state.tcCmd,
			}
			for _, mi := range state.multusInterfaces {
				podStatus.MultusInterfaces = append(podStatus.MultusInterfaces,
					tcv1alpha1.InjectedInterfaceStatus{
						NADName:   mi.nadName,
						Interface: mi.ifaceName,
						DelayMs:   mi.delayMs,
						TCCommand: mi.tcCmd,
					})
			}
			thisNodeDetails = append(thisNodeDetails, podStatus)
		}
		for attempt := 0; attempt < 5; attempt++ {
			if attempt > 0 {
				time.Sleep(100 * time.Millisecond)
			}
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

// resolveMultusInterfaces parses the Multus networks-status annotation on the pod and
// returns the interfaces that match any of the requested NAD names.
// It logs the reason when no interfaces are resolved so operators can diagnose mismatches.
func resolveMultusInterfaces(logger logr.Logger, pod *corev1.Pod, nadNames []string) []multusDesiredIface {
	if len(nadNames) == 0 {
		return nil
	}
	raw := pod.Annotations[multusNetworkStatusAnnotation]
	if raw == "" {
		logger.Info("multus networks-status annotation absent on pod; cannot resolve multus interfaces",
			"pod", pod.Name, "namespace", pod.Namespace,
			"annotation", multusNetworkStatusAnnotation)
		return nil
	}
	var statuses []multusNetworkStatus
	if err := json.Unmarshal([]byte(raw), &statuses); err != nil {
		logger.Error(err, "failed to parse multus networks-status annotation",
			"pod", pod.Name, "namespace", pod.Namespace)
		return nil
	}
	// Collect the NAD names present in the annotation for diagnostic logging.
	annotationNames := make([]string, 0, len(statuses))
	for _, s := range statuses {
		annotationNames = append(annotationNames, s.Name)
	}
	var result []multusDesiredIface
	for _, s := range statuses {
		if s.Interface == "" {
			continue
		}
		for _, nadName := range nadNames {
			if nadMatches(s.Name, nadName) {
				logger.V(1).Info("multus interface resolved",
					"pod", pod.Name, "nad", s.Name, "iface", s.Interface)
				result = append(result, multusDesiredIface{
					nadName:   s.Name,
					ifaceName: s.Interface,
				})
				break
			}
		}
	}
	if len(result) == 0 {
		logger.Info("no multus annotation entries matched the requested NAD names",
			"pod", pod.Name, "namespace", pod.Namespace,
			"requested", nadNames, "annotationNames", annotationNames)
	}
	return result
}

// nadMatches returns true if annotationName (from the Multus annotation, e.g.
// "default/mynetwork") matches the user-specified nadName.
// Supports "namespace/name" (exact) and "name" (match any namespace) forms.
func nadMatches(annotationName, nadName string) bool {
	if annotationName == nadName {
		return true
	}
	// If the user-specified name contains no slash, match the name part only.
	if !strings.Contains(nadName, "/") {
		if _, name, found := strings.Cut(annotationName, "/"); found {
			return name == nadName
		}
	}
	return false
}

// multusIfaceSetsEqual returns true if the existing Multus injected state exactly
// matches the desired Multus interfaces (same set of NADs, same interface names,
// and the same delayMs).
func multusIfaceSetsEqual(existing []multusInjectedState, desired []multusDesiredIface, delayMs int32) bool {
	if len(existing) != len(desired) {
		return false
	}
	em := make(map[string]multusInjectedState, len(existing))
	for _, mi := range existing {
		em[mi.nadName] = mi
	}
	for _, dmi := range desired {
		emi, ok := em[dmi.nadName]
		if !ok || emi.ifaceName != dmi.ifaceName || emi.delayMs != delayMs {
			return false
		}
	}
	return true
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
