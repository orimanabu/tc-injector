package controller

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	tcv1alpha1 "github.com/tc-injector/tc-injector/pkg/api/v1alpha1"
)

// ---- fakes ----

// fakePodFinder maps containerID -> netns path.
// Returns an error for unknown container IDs.
type fakePodFinder struct {
	mu           sync.Mutex
	netnsMapping map[string]string // containerID -> /proc/<pid>/ns/net
}

func newFakePodFinder(m map[string]string) *fakePodFinder {
	return &fakePodFinder{netnsMapping: m}
}

func (f *fakePodFinder) FindNetnsPath(_ context.Context, containerID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if path, ok := f.netnsMapping[containerID]; ok {
		return path, nil
	}
	return "", fmt.Errorf("no netns mapping for container %q", containerID)
}

// fakeTCApplier records ApplyInNetns and RemoveInNetns calls.
type fakeTCApplier struct {
	mu           sync.Mutex
	appliedNetns map[string]int32 // "netnsPath:iface" -> last applied delayMs
	removedNetns []string         // "netnsPath:iface"
	applyErr     error
	removeErr    error
}

func newFakeTCApplier() *fakeTCApplier {
	return &fakeTCApplier{
		appliedNetns: make(map[string]int32),
	}
}

func (f *fakeTCApplier) ApplyInNetns(netnsPath, iface string, delayMs int32) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return "", f.applyErr
	}
	key := netnsPath + ":" + iface
	f.appliedNetns[key] = delayMs
	return fmt.Sprintf("nsenter --net=%s -- tc qdisc replace dev %s root handle 1: netem delay %dms",
		netnsPath, iface, delayMs), nil
}

func (f *fakeTCApplier) RemoveInNetns(netnsPath, iface string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	key := netnsPath + ":" + iface
	delete(f.appliedNetns, key)
	f.removedNetns = append(f.removedNetns, key)
	return nil
}

func (f *fakeTCApplier) isAppliedInNetns(netnsPath, iface string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.appliedNetns[netnsPath+":"+iface]
	return ok
}

func (f *fakeTCApplier) wasRemovedInNetns(netnsPath, iface string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := netnsPath + ":" + iface
	for _, r := range f.removedNetns {
		if r == key {
			return true
		}
	}
	return false
}

// ---- test helpers ----

var testScheme *runtime.Scheme

func init() {
	testScheme = runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(testScheme)
	_ = tcv1alpha1.AddToScheme(testScheme)
}

func buildReconciler(
	t *testing.T,
	objs []client.Object,
	finder *fakePodFinder,
	applier *fakeTCApplier,
) (*Reconciler, *fake.ClientBuilder) {
	t.Helper()

	builder := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithStatusSubresource(&tcv1alpha1.TCInjector{}).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).
		WithObjects(objs...)

	c := builder.Build()

	r := &Reconciler{
		Client:    c,
		Scheme:    testScheme,
		NodeName:  "node-1",
		Finder:    finder,
		TCApplier: applier,
	}
	return r, builder
}

// readyPod returns a Pod that is Running with all containers ready.
func readyPod(name, namespace, nodeName, containerID string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID("uid-" + name),
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{ContainerID: containerID, Ready: true},
			},
		},
	}
}

func tcInjector(name string, rules []tcv1alpha1.DelayRule) *tcv1alpha1.TCInjector {
	return &tcv1alpha1.TCInjector{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       tcv1alpha1.TCInjectorSpec{Rules: rules},
	}
}

func tcInjectorWithRotation(name string, rules []tcv1alpha1.DelayRule, enabled bool, interval *metav1.Duration) *tcv1alpha1.TCInjector {
	return &tcv1alpha1.TCInjector{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: tcv1alpha1.TCInjectorSpec{
			Rules:                       rules,
			EnablePeriodicDelayRotation: enabled,
			DelayInterval:               interval,
		},
	}
}

func durationPtr(d time.Duration) *metav1.Duration {
	return &metav1.Duration{Duration: d}
}

func boolPtr(b bool) *bool { return &b }

func reconcileReq(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: client.ObjectKey{Name: name}}
}

// testNetns returns a predictable netns path for use in tests.
func testNetns(id string) string { return "/proc/test/" + id + "/ns/net" }

// ---- Reconcile tests ----

func TestReconcile_NoTCInjectors(t *testing.T) {
	pod := readyPod("pod1", "default", "node-1", "containerd://aaa", map[string]string{"app": "x"})
	finder := newFakePodFinder(map[string]string{"containerd://aaa": testNetns("aaa")})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq(""))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if len(applier.appliedNetns) != 0 {
		t.Errorf("expected no tc rules applied, got %v", applier.appliedNetns)
	}
}

func TestReconcile_AppliesRuleToMatchingPod(t *testing.T) {
	const containerID = "containerd://bbb"
	pod := readyPod("pod1", "default", "node-1", containerID,
		map[string]string{"app": "backend"})
	injector := tcInjector("test", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			MinDelay: 50,
			MaxDelay: 50,
		},
	})
	ns := testNetns("bbb")
	finder := newFakePodFinder(map[string]string{containerID: ns})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if !applier.isAppliedInNetns(ns, "eth0") {
		t.Errorf("expected tc rule applied inside netns %s on eth0", ns)
	}
	if delay := applier.appliedNetns[ns+":eth0"]; delay != 50 {
		t.Errorf("delay = %d, want 50", delay)
	}
}

func TestReconcile_SkipsNonMatchingPod(t *testing.T) {
	pod := readyPod("pod1", "default", "node-1", "containerd://ccc",
		map[string]string{"app": "frontend"})
	injector := tcInjector("test", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			MinDelay: 10,
			MaxDelay: 100,
		},
	})
	finder := newFakePodFinder(map[string]string{"containerd://ccc": testNetns("ccc")})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if len(applier.appliedNetns) != 0 {
		t.Errorf("expected no tc rules applied, got %v", applier.appliedNetns)
	}
}

func TestReconcile_SkipsPodOnDifferentNode(t *testing.T) {
	pod := readyPod("pod1", "default", "node-2", "containerd://ddd",
		map[string]string{"app": "backend"})
	injector := tcInjector("test", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			MinDelay: 10,
			MaxDelay: 10,
		},
	})
	finder := newFakePodFinder(map[string]string{"containerd://ddd": testNetns("ddd")})
	applier := newFakeTCApplier()
	// Reconciler is on node-1; pod is on node-2.
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if len(applier.appliedNetns) != 0 {
		t.Errorf("expected no rules for pods on other nodes")
	}
}

func TestReconcile_SkipsNotReadyPod(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod1", Namespace: "default",
			UID:    "uid-pod1",
			Labels: map[string]string{"app": "backend"},
		},
		Spec:   corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	injector := tcInjector("test", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			MinDelay: 10, MaxDelay: 10,
		},
	})
	finder := newFakePodFinder(map[string]string{})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if len(applier.appliedNetns) != 0 {
		t.Error("expected no tc rules for non-ready pod")
	}
}

func TestReconcile_RemovesRuleWhenPodNoLongerMatches(t *testing.T) {
	const containerID = "containerd://eee"
	ns := testNetns("eee")
	pod := readyPod("pod1", "default", "node-1", containerID,
		map[string]string{"app": "backend"})
	injector := tcInjector("test", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			MinDelay: 20, MaxDelay: 20,
		},
	})
	finder := newFakePodFinder(map[string]string{containerID: ns})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("first Reconcile error: %v", err)
	}
	if !applier.isAppliedInNetns(ns, "eth0") {
		t.Fatal("expected rule applied after first reconcile")
	}

	// Delete the TCInjector and reconcile again → rule must be removed.
	if err := r.Client.Delete(context.Background(), injector); err != nil {
		t.Fatalf("delete injector: %v", err)
	}

	_, err = r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("second Reconcile error: %v", err)
	}
	if applier.isAppliedInNetns(ns, "eth0") {
		t.Error("expected rule removed after TCInjector deletion")
	}
	if !applier.wasRemovedInNetns(ns, "eth0") {
		t.Error("expected RemoveInNetns called for eth0")
	}
}

func TestReconcile_MultipleRulesLastWins(t *testing.T) {
	const containerID = "containerd://fff"
	ns := testNetns("fff")
	pod := readyPod("pod1", "default", "node-1", containerID,
		map[string]string{"app": "backend", "tier": "slow"})
	injector := tcInjector("test", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			MinDelay: 10, MaxDelay: 10,
		},
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "slow"}},
			MinDelay: 200, MaxDelay: 200,
		},
	})
	finder := newFakePodFinder(map[string]string{containerID: ns})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	// Both rules match; map iteration order is non-deterministic, so we only
	// assert the final delay is one of the two defined values.
	delay := applier.appliedNetns[ns+":eth0"]
	if delay != 10 && delay != 200 {
		t.Errorf("delay = %d, want 10 or 200", delay)
	}
}

func TestReconcile_PodFinderError_SkipsPod(t *testing.T) {
	pod := readyPod("pod1", "default", "node-1", "containerd://ggg",
		map[string]string{"app": "backend"})
	injector := tcInjector("test", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			MinDelay: 10, MaxDelay: 10,
		},
	})
	// Finder has no mapping for this container ID → will error.
	finder := newFakePodFinder(map[string]string{})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	// Should not return an error (just logs and skips the pod).
	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile should not fail when pod finder fails: %v", err)
	}
	if len(applier.appliedNetns) != 0 {
		t.Error("expected no rules applied when pod finder fails")
	}
}

func TestReconcile_NoRequeueWhenNoTCInjectors(t *testing.T) {
	r, _ := buildReconciler(t, nil, newFakePodFinder(nil), newFakeTCApplier())
	result, err := r.Reconcile(context.Background(), reconcileReq(""))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 (no requeue)", result.RequeueAfter)
	}
}

// ---- periodic delay rotation tests ----

func TestReconcile_PeriodicRotation_DisabledByDefault_NoRequeue(t *testing.T) {
	injector := tcInjector("test", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			MinDelay: 10, MaxDelay: 10,
		},
	})
	r, _ := buildReconciler(t, []client.Object{injector}, newFakePodFinder(nil), newFakeTCApplier())

	result, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 when enablePeriodicDelayRotation is false", result.RequeueAfter)
	}
}

func TestReconcile_PeriodicRotation_ExplicitlyDisabled_NoRequeue(t *testing.T) {
	injector := tcInjectorWithRotation("test", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			MinDelay: 10, MaxDelay: 10,
		},
	}, false, durationPtr(5*time.Second))
	r, _ := buildReconciler(t, []client.Object{injector}, newFakePodFinder(nil), newFakeTCApplier())

	result, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 when enablePeriodicDelayRotation is false", result.RequeueAfter)
	}
}

func TestReconcile_PeriodicRotation_Enabled_DefaultInterval(t *testing.T) {
	injector := tcInjectorWithRotation("test", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			MinDelay: 10, MaxDelay: 10,
		},
	}, true, nil)
	r, _ := buildReconciler(t, []client.Object{injector}, newFakePodFinder(nil), newFakeTCApplier())

	result, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %v, want 30s (default)", result.RequeueAfter)
	}
}

func TestReconcile_PeriodicRotation_Enabled_CustomInterval(t *testing.T) {
	injector := tcInjectorWithRotation("test", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			MinDelay: 10, MaxDelay: 10,
		},
	}, true, durationPtr(2*time.Minute))
	r, _ := buildReconciler(t, []client.Object{injector}, newFakePodFinder(nil), newFakeTCApplier())

	result, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if result.RequeueAfter != 2*time.Minute {
		t.Errorf("RequeueAfter = %v, want 2m", result.RequeueAfter)
	}
}

func TestReconcile_PeriodicRotation_MultipleInjectors_MinIntervalUsed(t *testing.T) {
	injectorA := tcInjectorWithRotation("injector-a", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}},
			MinDelay: 10, MaxDelay: 10,
		},
	}, true, durationPtr(1*time.Minute))
	injectorB := tcInjectorWithRotation("injector-b", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "b"}},
			MinDelay: 20, MaxDelay: 20,
		},
	}, true, durationPtr(15*time.Second))
	r, _ := buildReconciler(t, []client.Object{injectorA, injectorB}, newFakePodFinder(nil), newFakeTCApplier())

	result, err := r.Reconcile(context.Background(), reconcileReq("injector-a"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Errorf("RequeueAfter = %v, want 15s (minimum of 60s and 15s)", result.RequeueAfter)
	}
}

func TestReconcile_PeriodicRotation_OnlyEnabledInjectorCounted(t *testing.T) {
	enabled := tcInjectorWithRotation("enabled", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}},
			MinDelay: 10, MaxDelay: 10,
		},
	}, true, durationPtr(45*time.Second))
	disabled := tcInjectorWithRotation("disabled", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "b"}},
			MinDelay: 20, MaxDelay: 20,
		},
	}, false, durationPtr(5*time.Second))
	r, _ := buildReconciler(t, []client.Object{enabled, disabled}, newFakePodFinder(nil), newFakeTCApplier())

	result, err := r.Reconcile(context.Background(), reconcileReq("enabled"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if result.RequeueAfter != 45*time.Second {
		t.Errorf("RequeueAfter = %v, want 45s (disabled injector must be ignored)", result.RequeueAfter)
	}
}

func TestReconcile_PeriodicRotation_ZeroInterval_FallsBackToDefault(t *testing.T) {
	injector := tcInjectorWithRotation("test", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			MinDelay: 10, MaxDelay: 10,
		},
	}, true, durationPtr(0))
	r, _ := buildReconciler(t, []client.Object{injector}, newFakePodFinder(nil), newFakeTCApplier())

	result, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %v, want 30s when delayInterval is zero", result.RequeueAfter)
	}
}

func TestReconcile_PeriodicRotation_Enabled_AppliesDelay(t *testing.T) {
	const containerID = "containerd://rot1"
	ns := testNetns("rot1")
	pod := readyPod("pod1", "default", "node-1", containerID,
		map[string]string{"app": "backend"})
	injector := tcInjectorWithRotation("test", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			MinDelay: 100, MaxDelay: 100,
		},
	}, true, durationPtr(10*time.Second))
	finder := newFakePodFinder(map[string]string{containerID: ns})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	result, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if !applier.isAppliedInNetns(ns, "eth0") {
		t.Error("expected tc rule applied to eth0 inside pod netns")
	}
	if applier.appliedNetns[ns+":eth0"] != 100 {
		t.Errorf("delay = %d, want 100", applier.appliedNetns[ns+":eth0"])
	}
	if result.RequeueAfter != 10*time.Second {
		t.Errorf("RequeueAfter = %v, want 10s", result.RequeueAfter)
	}
}

// ---- injectPrimaryInterface tests ----

func TestReconcile_InjectPrimaryInterface_Default_AppliesPrimary(t *testing.T) {
	// nil InjectPrimaryInterface (default) must apply tc to eth0 inside the pod netns.
	const containerID = "containerd://ipi1"
	ns := testNetns("ipi1")
	pod := readyPod("pod1", "default", "node-1", containerID,
		map[string]string{"app": "worker"})
	injector := tcInjector("test", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
			MinDelay: 50, MaxDelay: 50,
			// InjectPrimaryInterface: nil → treated as true
		},
	})
	finder := newFakePodFinder(map[string]string{containerID: ns})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if !applier.isAppliedInNetns(ns, "eth0") {
		t.Error("expected tc rule applied to eth0 (default injectPrimaryInterface)")
	}
}

func TestReconcile_InjectPrimaryInterface_True_AppliesPrimary(t *testing.T) {
	// Explicit true must apply tc to eth0.
	const containerID = "containerd://ipi2"
	ns := testNetns("ipi2")
	pod := readyPod("pod1", "default", "node-1", containerID,
		map[string]string{"app": "worker"})
	injector := tcInjector("test", []tcv1alpha1.DelayRule{
		{
			Selector:               metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
			MinDelay:               40, MaxDelay: 40,
			InjectPrimaryInterface: boolPtr(true),
		},
	})
	finder := newFakePodFinder(map[string]string{containerID: ns})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if !applier.isAppliedInNetns(ns, "eth0") {
		t.Error("expected tc rule applied to eth0 (explicit injectPrimaryInterface=true)")
	}
}

func TestReconcile_InjectPrimaryInterface_False_SkipsPrimary(t *testing.T) {
	// injectPrimaryInterface: false with no multusNetworks → no tc applied at all.
	const containerID = "containerd://ipi3"
	ns := testNetns("ipi3")
	pod := readyPod("pod1", "default", "node-1", containerID,
		map[string]string{"app": "worker"})
	injector := tcInjector("test", []tcv1alpha1.DelayRule{
		{
			Selector:               metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
			MinDelay:               30, MaxDelay: 30,
			InjectPrimaryInterface: boolPtr(false),
		},
	})
	finder := newFakePodFinder(map[string]string{containerID: ns})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if applier.isAppliedInNetns(ns, "eth0") {
		t.Error("expected eth0 to be skipped when injectPrimaryInterface=false")
	}
	if len(applier.appliedNetns) != 0 {
		t.Errorf("expected no tc rules applied at all, got %v", applier.appliedNetns)
	}
}

func TestReconcile_InjectPrimaryInterface_False_MultusOnly(t *testing.T) {
	// injectPrimaryInterface: false + multusNetworks → only Multus interface gets tc,
	// eth0 is untouched.
	const containerID = "containerd://ipi4"
	ns := testNetns("ipi4")
	const multusAnnotation = `[{"name":"default/mynetwork","interface":"net1"}]`

	pod := readyPod("pod1", "default", "node-1", containerID,
		map[string]string{"app": "multus-only"})
	pod.Annotations = map[string]string{
		multusNetworkStatusAnnotation: multusAnnotation,
	}
	injector := tcInjector("test", []tcv1alpha1.DelayRule{
		{
			Selector:               metav1.LabelSelector{MatchLabels: map[string]string{"app": "multus-only"}},
			MinDelay:               60, MaxDelay: 60,
			MultusNetworks:         []string{"default/mynetwork"},
			InjectPrimaryInterface: boolPtr(false),
		},
	})
	finder := newFakePodFinder(map[string]string{containerID: ns})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if applier.isAppliedInNetns(ns, "eth0") {
		t.Error("eth0 should be skipped when injectPrimaryInterface=false")
	}
	if !applier.isAppliedInNetns(ns, "net1") {
		t.Error("expected tc rule applied inside netns for Multus interface net1")
	}
	if applier.appliedNetns[ns+":net1"] != 60 {
		t.Errorf("Multus delay = %d, want 60", applier.appliedNetns[ns+":net1"])
	}
}

func TestReconcile_InjectPrimaryInterface_ToggleFalse_RemovesPrimary(t *testing.T) {
	// First reconcile: injectPrimaryInterface=true → eth0 tc applied.
	// Second reconcile: injectPrimaryInterface=false → eth0 tc removed.
	const containerID = "containerd://ipi5"
	ns := testNetns("ipi5")

	pod := readyPod("pod1", "default", "node-1", containerID,
		map[string]string{"app": "worker"})
	injector := tcInjector("test", []tcv1alpha1.DelayRule{
		{
			Selector:               metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
			MinDelay:               25, MaxDelay: 25,
			InjectPrimaryInterface: boolPtr(true),
		},
	})
	finder := newFakePodFinder(map[string]string{containerID: ns})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	if _, err := r.Reconcile(context.Background(), reconcileReq("test")); err != nil {
		t.Fatalf("first Reconcile error: %v", err)
	}
	if !applier.isAppliedInNetns(ns, "eth0") {
		t.Fatal("expected eth0 to be injected after first reconcile")
	}

	// Update the injector to disable primary injection.
	var current tcv1alpha1.TCInjector
	if err := r.Client.Get(context.Background(), client.ObjectKey{Name: "test"}, &current); err != nil {
		t.Fatalf("Get injector: %v", err)
	}
	current.Spec.Rules[0].InjectPrimaryInterface = boolPtr(false)
	if err := r.Client.Update(context.Background(), &current); err != nil {
		t.Fatalf("Update injector: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), reconcileReq("test")); err != nil {
		t.Fatalf("second Reconcile error: %v", err)
	}
	if applier.isAppliedInNetns(ns, "eth0") {
		t.Error("expected eth0 tc to be removed after injectPrimaryInterface toggled to false")
	}
	if !applier.wasRemovedInNetns(ns, "eth0") {
		t.Error("expected RemoveInNetns to be called for eth0")
	}
}

// ---- Multus interface tests ----

func TestReconcile_MultusInterface_Applied(t *testing.T) {
	const containerID = "containerd://mul1"
	ns := testNetns("mul1")
	const multusAnnotation = `[{"name":"default/mynetwork","interface":"net1"}]`

	pod := readyPod("pod1", "default", "node-1", containerID,
		map[string]string{"app": "worker"})
	pod.Annotations = map[string]string{
		multusNetworkStatusAnnotation: multusAnnotation,
	}
	injector := tcInjector("test", []tcv1alpha1.DelayRule{
		{
			Selector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "worker"}},
			MinDelay:       80, MaxDelay: 80,
			MultusNetworks: []string{"default/mynetwork"},
		},
	})
	finder := newFakePodFinder(map[string]string{containerID: ns})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	// Both primary and Multus should be applied in the same netns.
	if !applier.isAppliedInNetns(ns, "eth0") {
		t.Error("expected tc rule applied to eth0 (primary)")
	}
	if !applier.isAppliedInNetns(ns, "net1") {
		t.Error("expected tc rule applied to net1 (Multus)")
	}
	if applier.appliedNetns[ns+":eth0"] != 80 {
		t.Errorf("primary delay = %d, want 80", applier.appliedNetns[ns+":eth0"])
	}
	if applier.appliedNetns[ns+":net1"] != 80 {
		t.Errorf("multus delay = %d, want 80", applier.appliedNetns[ns+":net1"])
	}
}

// ---- nadMatches tests ----

func TestNadMatches(t *testing.T) {
	tests := []struct {
		annotationName string
		nadName        string
		want           bool
	}{
		{"default/mynetwork", "default/mynetwork", true},
		{"default/mynetwork", "mynetwork", true},
		{"kube-system/mynetwork", "mynetwork", true},
		{"default/mynetwork", "other", false},
		{"default/mynetwork", "kube-system/mynetwork", false},
		{"mynetwork", "mynetwork", true},
		{"mynetwork", "other", false},
	}
	for _, tt := range tests {
		t.Run(tt.annotationName+"~"+tt.nadName, func(t *testing.T) {
			if got := nadMatches(tt.annotationName, tt.nadName); got != tt.want {
				t.Errorf("nadMatches(%q, %q) = %v, want %v",
					tt.annotationName, tt.nadName, got, tt.want)
			}
		})
	}
}

// ---- resolveMultusInterfaces tests ----

func TestResolveMultusInterfaces(t *testing.T) {
	logger := logr.Discard()

	t.Run("empty nadNames returns nil", func(t *testing.T) {
		pod := &corev1.Pod{}
		if got := resolveMultusInterfaces(logger, pod, nil); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("annotation absent returns nil", func(t *testing.T) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}
		if got := resolveMultusInterfaces(logger, pod, []string{"mynetwork"}); got != nil {
			t.Errorf("expected nil when annotation absent, got %v", got)
		}
	})

	t.Run("invalid JSON returns nil", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p", Namespace: "default",
				Annotations: map[string]string{multusNetworkStatusAnnotation: "not-json"},
			},
		}
		if got := resolveMultusInterfaces(logger, pod, []string{"mynetwork"}); got != nil {
			t.Errorf("expected nil on JSON error, got %v", got)
		}
	})

	t.Run("exact namespace/name match", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p", Namespace: "default",
				Annotations: map[string]string{
					multusNetworkStatusAnnotation: `[{"name":"default/mynetwork","interface":"net1"}]`,
				},
			},
		}
		got := resolveMultusInterfaces(logger, pod, []string{"default/mynetwork"})
		if len(got) != 1 {
			t.Fatalf("expected 1 result, got %d", len(got))
		}
		if got[0].nadName != "default/mynetwork" || got[0].ifaceName != "net1" {
			t.Errorf("unexpected result: %+v", got[0])
		}
	})

	t.Run("name-only match across namespace", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p", Namespace: "default",
				Annotations: map[string]string{
					multusNetworkStatusAnnotation: `[{"name":"kube-system/mynetwork","interface":"net2"}]`,
				},
			},
		}
		got := resolveMultusInterfaces(logger, pod, []string{"mynetwork"})
		if len(got) != 1 {
			t.Fatalf("expected 1 result, got %d", len(got))
		}
		if got[0].ifaceName != "net2" {
			t.Errorf("expected interface net2, got %q", got[0].ifaceName)
		}
	})

	t.Run("no match returns nil", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p", Namespace: "default",
				Annotations: map[string]string{
					multusNetworkStatusAnnotation: `[{"name":"default/other","interface":"net1"}]`,
				},
			},
		}
		if got := resolveMultusInterfaces(logger, pod, []string{"mynetwork"}); got != nil {
			t.Errorf("expected nil for no match, got %v", got)
		}
	})

	t.Run("skips entry with empty interface name", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p", Namespace: "default",
				Annotations: map[string]string{
					multusNetworkStatusAnnotation: `[{"name":"default/mynetwork","interface":""},{"name":"default/mynetwork","interface":"net1"}]`,
				},
			},
		}
		got := resolveMultusInterfaces(logger, pod, []string{"default/mynetwork"})
		if len(got) != 1 {
			t.Fatalf("expected 1 result (empty interface skipped), got %d", len(got))
		}
		if got[0].ifaceName != "net1" {
			t.Errorf("expected interface net1, got %q", got[0].ifaceName)
		}
	})
}

// ---- helper function tests ----

func TestIsPodReady(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "running all ready",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{Ready: true},
						{Ready: true},
					},
				},
			},
			want: true,
		},
		{
			name: "running one not ready",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{Ready: true},
						{Ready: false},
					},
				},
			},
			want: false,
		},
		{
			name: "pending phase",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{Phase: corev1.PodPending},
			},
			want: false,
		},
		{
			name: "running no container statuses",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPodReady(tt.pod); got != tt.want {
				t.Errorf("isPodReady() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFirstReadyContainerID(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "first ready",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{ContainerID: "containerd://abc", Ready: true},
			}}},
			want: "containerd://abc",
		},
		{
			name: "skips not-ready then returns ready",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{ContainerID: "containerd://bad", Ready: false},
				{ContainerID: "containerd://good", Ready: true},
			}}},
			want: "containerd://good",
		},
		{
			name: "skips empty container ID",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{ContainerID: "", Ready: true},
				{ContainerID: "containerd://ok", Ready: true},
			}}},
			want: "containerd://ok",
		},
		{
			name: "none ready",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{ContainerID: "containerd://abc", Ready: false},
			}}},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstReadyContainerID(tt.pod); got != tt.want {
				t.Errorf("firstReadyContainerID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFindPodByUID(t *testing.T) {
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{UID: "uid-a"}},
		{ObjectMeta: metav1.ObjectMeta{UID: "uid-b"}},
	}
	if p := findPodByUID(pods, "uid-a"); p == nil || string(p.UID) != "uid-a" {
		t.Error("findPodByUID failed to find uid-a")
	}
	if p := findPodByUID(pods, "uid-missing"); p != nil {
		t.Error("findPodByUID should return nil for unknown UID")
	}
}

func TestSetCondition(t *testing.T) {
	injector := &tcv1alpha1.TCInjector{}

	cond := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "R", Message: "m"}
	setCondition(injector, cond)
	if len(injector.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(injector.Status.Conditions))
	}

	// Upsert the same type → should replace, not append.
	cond.Message = "updated"
	setCondition(injector, cond)
	if len(injector.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after upsert, got %d", len(injector.Status.Conditions))
	}
	if injector.Status.Conditions[0].Message != "updated" {
		t.Errorf("condition message = %q, want \"updated\"", injector.Status.Conditions[0].Message)
	}

	// Adding a different type → should append.
	setCondition(injector, metav1.Condition{Type: "Other", Status: metav1.ConditionFalse, Reason: "R"})
	if len(injector.Status.Conditions) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(injector.Status.Conditions))
	}
}
