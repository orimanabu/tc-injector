// Package tc wraps the Linux tc(8) command to apply netem delay rules
// on network interfaces.
package tc

import (
	"fmt"
	"math/rand"
	"os/exec"
	"strings"
	"time"
)

// Rule describes a delay to apply to an interface.
type Rule struct {
	// Iface is the host-side veth interface name (e.g. "veth1a2b3c").
	Iface string
	// DelayMs is the exact delay in milliseconds to inject.
	DelayMs int32
}

// RandomDelay returns a random delay between minMs and maxMs (inclusive).
func RandomDelay(minMs, maxMs int32) int32 {
	if minMs >= maxMs {
		return minMs
	}
	//nolint:gosec // non-cryptographic random is fine for delay jitter
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	return minMs + r.Int31n(maxMs-minMs+1)
}

// Apply installs or replaces a netem delay qdisc on the given interface.
// Calling Apply on an interface that already has a rule replaces it atomically.
func Apply(iface string, delayMs int32) error {
	if err := validateIface(iface); err != nil {
		return err
	}

	// Try to replace first (idempotent if qdisc already exists).
	args := []string{
		"qdisc", "replace", "dev", iface,
		"root", "handle", "1:", "netem",
		"delay", fmt.Sprintf("%dms", delayMs),
	}
	if out, err := runCmd("tc", args...); err != nil {
		// replace fails on a pristine interface; fall back to add.
		addArgs := []string{
			"qdisc", "add", "dev", iface,
			"root", "handle", "1:", "netem",
			"delay", fmt.Sprintf("%dms", delayMs),
		}
		if out2, err2 := runCmd("tc", addArgs...); err2 != nil {
			return fmt.Errorf("tc qdisc add/replace on %s: %w (replace output: %s, add output: %s)",
				iface, err2, out, out2)
		}
	}
	return nil
}

// Remove deletes the root qdisc from the given interface, restoring normal
// scheduling. It is a no-op if no qdisc is present.
func Remove(iface string) error {
	if err := validateIface(iface); err != nil {
		return err
	}
	args := []string{"qdisc", "del", "dev", iface, "root"}
	if out, err := runCmd("tc", args...); err != nil {
		// RTNETLINK answers: No such file or directory → qdisc was already absent.
		if strings.Contains(out, "RTNETLINK") || strings.Contains(out, "No such") {
			return nil
		}
		return fmt.Errorf("tc qdisc del on %s: %w (output: %s)", iface, err, out)
	}
	return nil
}

// ApplyInNetns installs or replaces a netem delay qdisc on iface inside the
// network namespace identified by netnsPath (a /proc/<pid>/ns/net path).
// It runs tc(8) via nsenter(1) and returns the command string that was executed.
// Calling ApplyInNetns on an interface that already has a rule replaces it atomically.
func ApplyInNetns(netnsPath, iface string, delayMs int32) (string, error) {
	if err := validateIface(iface); err != nil {
		return "", err
	}
	delay := fmt.Sprintf("%dms", delayMs)
	replaceArgs := []string{
		"--net=" + netnsPath, "--",
		"tc", "qdisc", "replace", "dev", iface,
		"root", "handle", "1:", "netem", "delay", delay,
	}
	tcCmd := fmt.Sprintf("nsenter --net=%s -- tc qdisc replace dev %s root handle 1: netem delay %s",
		netnsPath, iface, delay)
	if out, err := runCmd("nsenter", replaceArgs...); err != nil {
		// replace fails on a pristine interface; fall back to add.
		addArgs := []string{
			"--net=" + netnsPath, "--",
			"tc", "qdisc", "add", "dev", iface,
			"root", "handle", "1:", "netem", "delay", delay,
		}
		tcCmd = fmt.Sprintf("nsenter --net=%s -- tc qdisc add dev %s root handle 1: netem delay %s",
			netnsPath, iface, delay)
		if out2, err2 := runCmd("nsenter", addArgs...); err2 != nil {
			return "", fmt.Errorf("tc qdisc add/replace on %s in netns %s: %w (replace output: %s, add output: %s)",
				iface, netnsPath, err2, out, out2)
		}
	}
	return tcCmd, nil
}

// RemoveInNetns deletes the root qdisc from iface inside the given network namespace.
// It tolerates errors when the namespace or interface no longer exists (e.g. the pod
// has been deleted), making it safe to call during pod cleanup.
func RemoveInNetns(netnsPath, iface string) error {
	if err := validateIface(iface); err != nil {
		return err
	}
	args := []string{
		"--net=" + netnsPath, "--",
		"tc", "qdisc", "del", "dev", iface, "root",
	}
	if out, err := runCmd("nsenter", args...); err != nil {
		// Tolerate: qdisc absent, netns already gone (pod deleted), or interface missing.
		if strings.Contains(out, "RTNETLINK") ||
			strings.Contains(out, "No such") ||
			strings.Contains(out, "Cannot open network namespace") {
			return nil
		}
		return fmt.Errorf("tc qdisc del on %s in netns %s: %w (output: %s)", iface, netnsPath, err, out)
	}
	return nil
}

// Show returns the current tc qdisc configuration for an interface.
func Show(iface string) (string, error) {
	if err := validateIface(iface); err != nil {
		return "", err
	}
	out, err := runCmd("tc", "qdisc", "show", "dev", iface)
	return out, err
}

// runCmd is the exec backend. Tests may replace this to avoid calling tc(8).
var runCmd = func(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// validateIface guards against shell injection by rejecting interface names
// with characters outside the expected set.
func validateIface(iface string) error {
	if iface == "" {
		return fmt.Errorf("interface name must not be empty")
	}
	for _, c := range iface {
		if !isAlphanumericOrDash(c) {
			return fmt.Errorf("interface name %q contains invalid character %q", iface, c)
		}
	}
	return nil
}

func isAlphanumericOrDash(c rune) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '-' || c == '_' || c == '.'
}
