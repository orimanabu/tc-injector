package veth

import (
	"os"
	"path/filepath"
	"testing"

	criapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// ---- pidFromInfoMap ----

func TestPidFromInfoMap_ContainerdFormat(t *testing.T) {
	// containerd stores the pid inside an "info" key as a JSON blob.
	info := map[string]string{
		"info": `{"pid":12345,"sandboxID":"abc","runtimeSpec":{}}`,
	}
	pid, ok := pidFromInfoMap(info)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if pid != 12345 {
		t.Errorf("pid = %d, want 12345", pid)
	}
}

func TestPidFromInfoMap_CRIOFormat(t *testing.T) {
	// CRI-O also stores pid at the top level of the info JSON.
	info := map[string]string{
		"info": `{"pid":9999,"image":"registry.example.com/app:latest"}`,
	}
	pid, ok := pidFromInfoMap(info)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if pid != 9999 {
		t.Errorf("pid = %d, want 9999", pid)
	}
}

func TestPidFromInfoMap_DirectPidKey(t *testing.T) {
	// Some containerd builds expose pid as a direct string key.
	info := map[string]string{
		"pid": "7777",
	}
	pid, ok := pidFromInfoMap(info)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if pid != 7777 {
		t.Errorf("pid = %d, want 7777", pid)
	}
}

func TestPidFromInfoMap_NestedInfoKey(t *testing.T) {
	// Handles {"info": {"pid": N}} nesting.
	info := map[string]string{
		"info": `{"info":{"pid":3333}}`,
	}
	pid, ok := pidFromInfoMap(info)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if pid != 3333 {
		t.Errorf("pid = %d, want 3333", pid)
	}
}

func TestPidFromInfoMap_MissingKey(t *testing.T) {
	info := map[string]string{"other": "value"}
	_, ok := pidFromInfoMap(info)
	if ok {
		t.Fatal("expected ok=false for missing keys")
	}
}

func TestPidFromInfoMap_EmptyMap(t *testing.T) {
	_, ok := pidFromInfoMap(map[string]string{})
	if ok {
		t.Fatal("expected ok=false for empty map")
	}
}

func TestPidFromInfoMap_InvalidJSON(t *testing.T) {
	info := map[string]string{"info": `{not valid json}`}
	_, ok := pidFromInfoMap(info)
	if ok {
		t.Fatal("expected ok=false for invalid JSON")
	}
}

func TestPidFromInfoMap_ZeroPID(t *testing.T) {
	info := map[string]string{"info": `{"pid":0}`}
	_, ok := pidFromInfoMap(info)
	if ok {
		t.Fatal("expected ok=false when pid=0")
	}
}

func TestPidFromInfoMap_NegativePID(t *testing.T) {
	info := map[string]string{"info": `{"pid":-1}`}
	_, ok := pidFromInfoMap(info)
	if ok {
		t.Fatal("expected ok=false when pid<0")
	}
}

// ---- isNetnsPath ----

func TestIsNetnsPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/var/run/netns/crio-abc123", true},
		{"/proc/12345/ns/net", true},
		{"/var/run/netns/some-pod", true},
		{"/run/netns/custom", true},
		{"/proc/1/ns/net", true},
		{"/proc/1/ns/pid", false},
		{"/proc/1/ns/ipc", false},
		{"/sys/class/net/eth0", false},
		{"", false},
	}
	for _, tt := range tests {
		got := isNetnsPath(tt.path)
		if got != tt.want {
			t.Errorf("isNetnsPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// ---- netnsFromLinuxNamespaces ----

func TestNetnsFromLinuxNamespaces_CRIOPath(t *testing.T) {
	status := &criapi.PodSandboxStatus{
		Linux: &criapi.LinuxPodSandboxStatus{
			Namespaces: []*criapi.Namespace{
				{Path: "/proc/1/ns/pid"},
				{Path: "/var/run/netns/crio-abc"},
				{Path: "/proc/1/ns/ipc"},
			},
		},
	}
	path, ok := netnsFromLinuxNamespaces(status)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if path != "/var/run/netns/crio-abc" {
		t.Errorf("path = %q, want /var/run/netns/crio-abc", path)
	}
}

func TestNetnsFromLinuxNamespaces_ProcPath(t *testing.T) {
	status := &criapi.PodSandboxStatus{
		Linux: &criapi.LinuxPodSandboxStatus{
			Namespaces: []*criapi.Namespace{
				{Path: "/proc/5678/ns/net"},
			},
		},
	}
	path, ok := netnsFromLinuxNamespaces(status)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if path != "/proc/5678/ns/net" {
		t.Errorf("path = %q", path)
	}
}

func TestNetnsFromLinuxNamespaces_NoNetnsEntry(t *testing.T) {
	status := &criapi.PodSandboxStatus{
		Linux: &criapi.LinuxPodSandboxStatus{
			Namespaces: []*criapi.Namespace{
				{Path: "/proc/1/ns/pid"},
				{Path: "/proc/1/ns/ipc"},
			},
		},
	}
	_, ok := netnsFromLinuxNamespaces(status)
	if ok {
		t.Fatal("expected ok=false when no netns entry present")
	}
}

func TestNetnsFromLinuxNamespaces_NilStatus(t *testing.T) {
	_, ok := netnsFromLinuxNamespaces(nil)
	if ok {
		t.Fatal("expected ok=false for nil status")
	}
}

func TestNetnsFromLinuxNamespaces_NilLinux(t *testing.T) {
	_, ok := netnsFromLinuxNamespaces(&criapi.PodSandboxStatus{Linux: nil})
	if ok {
		t.Fatal("expected ok=false for nil Linux field")
	}
}

func TestNetnsFromLinuxNamespaces_EmptyPath(t *testing.T) {
	status := &criapi.PodSandboxStatus{
		Linux: &criapi.LinuxPodSandboxStatus{
			Namespaces: []*criapi.Namespace{
				{Path: ""},
			},
		},
	}
	_, ok := netnsFromLinuxNamespaces(status)
	if ok {
		t.Fatal("expected ok=false when path is empty")
	}
}

// ---- stripRuntimePrefix ----

func TestStripRuntimePrefix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"containerd://abc123def456", "abc123def456"},
		{"cri-o://abc123def456", "abc123def456"},
		{"docker://abc123def456", "abc123def456"},
		{"abc123def456", "abc123def456"},  // no prefix
		{"", ""},
	}
	for _, tt := range tests {
		got := stripRuntimePrefix(tt.input)
		if got != tt.want {
			t.Errorf("stripRuntimePrefix(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ---- procNetnsPath ----

func TestProcNetnsPath(t *testing.T) {
	got := procNetnsPath(12345)
	want := "/proc/12345/ns/net"
	if got != want {
		t.Errorf("procNetnsPath(12345) = %q, want %q", got, want)
	}
}

// ---- resolveCRISocket ----

func TestResolveCRISocket_Explicit(t *testing.T) {
	got, err := resolveCRISocket("/custom/path.sock")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/custom/path.sock" {
		t.Errorf("got %q, want /custom/path.sock", got)
	}
}

func TestResolveCRISocket_AutoDetect(t *testing.T) {
	// Create a temporary fake socket file and inject it as the first candidate.
	tmp := t.TempDir()
	fakeSock := filepath.Join(tmp, "containerd.sock")
	if err := os.WriteFile(fakeSock, nil, 0600); err != nil {
		t.Fatal(err)
	}

	prev := knownCRISockets
	knownCRISockets = []string{fakeSock}
	defer func() { knownCRISockets = prev }()

	got, err := resolveCRISocket("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != fakeSock {
		t.Errorf("got %q, want %q", got, fakeSock)
	}
}

func TestResolveCRISocket_FallsBackToSecondCandidate(t *testing.T) {
	tmp := t.TempDir()
	missingPath := filepath.Join(tmp, "missing.sock")
	presentPath := filepath.Join(tmp, "present.sock")
	if err := os.WriteFile(presentPath, nil, 0600); err != nil {
		t.Fatal(err)
	}

	prev := knownCRISockets
	knownCRISockets = []string{missingPath, presentPath}
	defer func() { knownCRISockets = prev }()

	got, err := resolveCRISocket("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != presentPath {
		t.Errorf("got %q, want %q", got, presentPath)
	}
}

func TestResolveCRISocket_NoCandidatesFound(t *testing.T) {
	prev := knownCRISockets
	knownCRISockets = []string{"/nonexistent/path1.sock", "/nonexistent/path2.sock"}
	defer func() { knownCRISockets = prev }()

	_, err := resolveCRISocket("")
	if err == nil {
		t.Fatal("expected error when no socket candidates exist")
	}
}
