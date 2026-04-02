// Package veth provides utilities to resolve a pod's network namespace path
// from its container ID via the CRI (Container Runtime Interface).
package veth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	criapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const procMountPath = "/proc"

// knownCRISockets lists candidate socket paths tried in order when no explicit
// socket is provided. containerd is tried first, then CRI-O.
var knownCRISockets = []string{
	"/run/containerd/containerd.sock",
	"/run/crio/crio.sock",
	"/var/run/crio/crio.sock",
}

// Finder resolves pod container IDs to the pod's network namespace path.
type Finder struct {
	criSocket string
	criClient criapi.RuntimeServiceClient
	conn      *grpc.ClientConn
}

// NewFinder creates a Finder connected to the given CRI socket.
// If criSocket is empty, it probes the well-known socket paths for containerd
// and CRI-O and uses the first one that exists on the filesystem.
func NewFinder(criSocket string) (*Finder, error) {
	socket, err := resolveCRISocket(criSocket)
	if err != nil {
		return nil, err
	}

	// grpc.NewClient was introduced in grpc-go v1.63.0; use grpc.Dial for v1.62.x.
	//nolint:staticcheck
	conn, err := grpc.Dial(
		"unix://"+socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to CRI socket %s: %w", socket, err)
	}

	return &Finder{
		criSocket: socket,
		criClient: criapi.NewRuntimeServiceClient(conn),
		conn:      conn,
	}, nil
}

// resolveCRISocket returns the first existing socket from candidates, or the
// caller-supplied value if non-empty.
func resolveCRISocket(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	for _, s := range knownCRISockets {
		if _, err := os.Stat(s); err == nil {
			return s, nil
		}
	}
	return "", fmt.Errorf("no CRI socket found; tried %v", knownCRISockets)
}

// Close releases the CRI gRPC connection.
func (f *Finder) Close() error {
	return f.conn.Close()
}

// FindNetnsPath returns the /proc/<pid>/ns/net path for the given container.
// The path can be passed to nsenter(1) to run commands inside the pod's network namespace.
// containerID should be in the form "containerd://<id>" or "cri-o://<id>".
func (f *Finder) FindNetnsPath(ctx context.Context, containerID string) (string, error) {
	id := stripRuntimePrefix(containerID)
	if id == "" {
		return "", fmt.Errorf("empty container ID after stripping runtime prefix: %q", containerID)
	}
	return f.resolveNetnsPath(ctx, id)
}

// resolveNetnsPath determines the network namespace path for the given container.
//
// Strategy (works for both containerd and CRI-O):
//  1. Call ContainerStatus(verbose=true) and extract the PID from the info map.
//     Both runtimes embed {"pid": N} somewhere in the verbose JSON.
//  2. If PID extraction fails, call PodSandboxStatus(verbose=true) and extract
//     the PID from the sandbox info map.
//
// Note: In cri-api v0.29, LinuxPodSandboxStatus.Namespaces only carries
// NamespaceOption (mode enum), not filesystem paths, so path-based lookup is
// not available in this API version.
func (f *Finder) resolveNetnsPath(ctx context.Context, containerID string) (string, error) {
	// --- Step 1: try PID from ContainerStatus ---
	csResp, err := f.criClient.ContainerStatus(ctx, &criapi.ContainerStatusRequest{
		ContainerId: containerID,
		Verbose:     true,
	})
	if err != nil {
		return "", fmt.Errorf("CRI ContainerStatus(%s): %w", containerID, err)
	}

	if pid, ok := pidFromInfoMap(csResp.GetInfo()); ok {
		return procNetnsPath(pid), nil
	}

	// --- Step 2: fall back to sandbox info map ---
	containers, err := f.criClient.ListContainers(ctx, &criapi.ListContainersRequest{
		Filter: &criapi.ContainerFilter{Id: containerID},
	})
	if err != nil {
		return "", fmt.Errorf("list containers: %w", err)
	}
	if len(containers.Containers) == 0 {
		return "", fmt.Errorf("container %s not found via CRI", containerID)
	}

	sandboxID := containers.Containers[0].PodSandboxId
	sbResp, err := f.criClient.PodSandboxStatus(ctx, &criapi.PodSandboxStatusRequest{
		PodSandboxId: sandboxID,
		Verbose:      true,
	})
	if err != nil {
		return "", fmt.Errorf("PodSandboxStatus(%s): %w", sandboxID, err)
	}

	if pid, ok := pidFromInfoMap(sbResp.GetInfo()); ok {
		return procNetnsPath(pid), nil
	}

	return "", fmt.Errorf("could not determine netns for sandbox %s (container %s)", sandboxID, containerID)
}

// pidFromInfoMap extracts the process PID from the CRI verbose info map.
//
// Both containerd and CRI-O encode the PID as a numeric field named "pid"
// inside a JSON blob stored under the "info" key of the info map:
//
//	containerd: info["info"] = `{"pid":12345,"sandboxID":"...","runtimeSpec":{...}}`
//	CRI-O:      info["info"] = `{"pid":12345,...}`
//
// Some containerd builds also set a top-level info["pid"] string directly.
func pidFromInfoMap(info map[string]string) (int, bool) {
	// Fast path: direct "pid" key (some containerd builds).
	if raw, ok := info["pid"]; ok {
		var pid int
		if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%d", &pid); err == nil && pid > 0 {
			return pid, true
		}
	}

	// General path: parse the "info" JSON blob.
	raw, ok := info["info"]
	if !ok || raw == "" {
		return 0, false
	}

	// Use a two-level struct to handle both flat and nested layouts.
	var blob struct {
		PID  int `json:"pid"`
		Info struct {
			PID int `json:"pid"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(raw), &blob); err != nil {
		return 0, false
	}
	if blob.PID > 0 {
		return blob.PID, true
	}
	if blob.Info.PID > 0 {
		return blob.Info.PID, true
	}
	return 0, false
}

// procNetnsPath returns the /proc path to the network namespace of a PID.
// The DaemonSet mounts the host's /proc at procMountPath.
func procNetnsPath(pid int) string {
	return fmt.Sprintf("%s/%d/ns/net", procMountPath, pid)
}

// stripRuntimePrefix removes scheme prefixes like "containerd://", "cri-o://",
// "docker://", leaving only the bare container ID.
func stripRuntimePrefix(containerID string) string {
	if idx := strings.Index(containerID, "://"); idx >= 0 {
		return containerID[idx+3:]
	}
	return containerID
}
