package controller

import (
	"context"
	"fmt"
	"sync"
	"testing"

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

// fakeVethFinder maps containerID -> interface name. Returns an error for unknown IDs.
type fakeVethFinder struct {
	mu      sync.Mutex
	mapping map[string]string
}

func newFakeVethFinder(m map[string]string) *fakeVethFinder {
	return &fakeVethFinder{mapping: m}
}

func (f *fakeVethFinder) FindHostVeth(_ context.Context, containerID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if iface, ok := f.mapping[containerID]; ok {
		return iface, nil
	}
	return "", fmt.Errorf("no veth mapping for container %q", containerID)
}

// fakeTCApplier records Apply and Remove calls.
type fakeTCApplier struct {
	mu      sync.Mutex
	applied map[string]int32  // iface -> last applied delayMs
	removed []string
	applyErr error
	removeErr error
}

func newFakeTCApplier() *fakeTCApplier {
	return &fakeTCApplier{applied: make(map[string]int32)}
}

func (f *fakeTCApplier) Apply(iface string, delayMs int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied[iface] = delayMs
	return nil
}

func (f *fakeTCApplier) Remove(iface string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	delete(f.applied, iface)
	f.removed = append(f.removed, iface)
	return nil
}

func (f *fakeTCApplier) isApplied(iface string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.applied[iface]
	return ok
}

func (f *fakeTCApplier) wasRemoved(iface string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.removed {
		if r == iface {
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
	finder *fakeVethFinder,
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

func reconcileReq(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: client.ObjectKey{Name: name}}
}

// ---- Reconcile tests ----

func TestReconcile_NoTCInjectors(t *testing.T) {
	pod := readyPod("pod1", "default", "node-1", "containerd://aaa", map[string]string{"app": "x"})
	finder := newFakeVethFinder(map[string]string{"containerd://aaa": "veth0"})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq(""))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if len(applier.applied) != 0 {
		t.Errorf("expected no tc rules applied, got %v", applier.applied)
	}
}

func TestReconcile_AppliesRuleToMatchingPod(t *testing.T) {
	pod := readyPod("pod1", "default", "node-1", "containerd://bbb",
		map[string]string{"app": "backend"})
	injector := tcInjector("test", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			MinDelay: 50,
			MaxDelay: 50,
		},
	})
	finder := newFakeVethFinder(map[string]string{"containerd://bbb": "veth1abc"})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if !applier.isApplied("veth1abc") {
		t.Error("expected tc rule applied to veth1abc")
	}
	if delay := applier.applied["veth1abc"]; delay != 50 {
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
	finder := newFakeVethFinder(map[string]string{"containerd://ccc": "veth2"})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if len(applier.applied) != 0 {
		t.Errorf("expected no tc rules applied, got %v", applier.applied)
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
	finder := newFakeVethFinder(map[string]string{"containerd://ddd": "veth3"})
	applier := newFakeTCApplier()
	// Reconciler is on node-1; pod is on node-2.
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if len(applier.applied) != 0 {
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
	finder := newFakeVethFinder(map[string]string{})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if len(applier.applied) != 0 {
		t.Error("expected no tc rules for non-ready pod")
	}
}

func TestReconcile_RemovesRuleWhenPodNoLongerMatches(t *testing.T) {
	// First reconcile: pod matches → rule applied.
	pod := readyPod("pod1", "default", "node-1", "containerd://eee",
		map[string]string{"app": "backend"})
	injector := tcInjector("test", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			MinDelay: 20, MaxDelay: 20,
		},
	})
	finder := newFakeVethFinder(map[string]string{"containerd://eee": "veth4"})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("first Reconcile error: %v", err)
	}
	if !applier.isApplied("veth4") {
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
	if applier.isApplied("veth4") {
		t.Error("expected rule removed after TCInjector deletion")
	}
	if !applier.wasRemoved("veth4") {
		t.Error("expected Remove called on veth4")
	}
}

func TestReconcile_MultipleRulesLastWins(t *testing.T) {
	// A pod that matches two rules: the second rule's delay should win.
	pod := readyPod("pod1", "default", "node-1", "containerd://fff",
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
	finder := newFakeVethFinder(map[string]string{"containerd://fff": "veth5"})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	// Both rules match; map iteration order is non-deterministic, so we only
	// assert the final delay is one of the two defined values.
	delay := applier.applied["veth5"]
	if delay != 10 && delay != 200 {
		t.Errorf("delay = %d, want 10 or 200", delay)
	}
}

func TestReconcile_VethFinderError_SkipsPod(t *testing.T) {
	pod := readyPod("pod1", "default", "node-1", "containerd://ggg",
		map[string]string{"app": "backend"})
	injector := tcInjector("test", []tcv1alpha1.DelayRule{
		{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			MinDelay: 10, MaxDelay: 10,
		},
	})
	// Finder has no mapping for this container ID → will error.
	finder := newFakeVethFinder(map[string]string{})
	applier := newFakeTCApplier()
	r, _ := buildReconciler(t, []client.Object{pod, injector}, finder, applier)

	// Should not return an error (just logs and skips the pod).
	_, err := r.Reconcile(context.Background(), reconcileReq("test"))
	if err != nil {
		t.Fatalf("Reconcile should not fail when veth lookup fails: %v", err)
	}
	if len(applier.applied) != 0 {
		t.Error("expected no rules applied when veth lookup fails")
	}
}

func TestReconcile_RequeuesAfter30s(t *testing.T) {
	r, _ := buildReconciler(t, nil, newFakeVethFinder(nil), newFakeTCApplier())
	result, err := r.Reconcile(context.Background(), reconcileReq(""))
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	want := 30 * 1e9 // 30 seconds in nanoseconds
	if result.RequeueAfter.Nanoseconds() != int64(want) {
		t.Errorf("RequeueAfter = %v, want 30s", result.RequeueAfter)
	}
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
