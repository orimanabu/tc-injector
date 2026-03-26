package tc

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// capturedCall records one invocation of runCmd.
type capturedCall struct {
	name string
	args []string
}

// fakeRunner replaces runCmd and records all calls.
// If an entry in responses matches the first non-"tc" arg (the sub-command),
// it returns the configured output/error; otherwise it returns ("", nil).
type fakeRunner struct {
	responses map[string]struct {
		out string
		err error
	}
	calls []capturedCall
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		responses: make(map[string]struct {
			out string
			err error
		}),
	}
}

func (f *fakeRunner) install() func() {
	prev := runCmd
	runCmd = f.run
	return func() { runCmd = prev }
}

func (f *fakeRunner) run(name string, args ...string) (string, error) {
	f.calls = append(f.calls, capturedCall{name: name, args: args})
	// Key on the tc sub-command, e.g. "add", "replace", "del", "show".
	key := ""
	for _, a := range args {
		if a == "add" || a == "replace" || a == "del" || a == "show" {
			key = a
			break
		}
	}
	if r, ok := f.responses[key]; ok {
		return r.out, r.err
	}
	return "", nil
}

func (f *fakeRunner) setResponse(subCmd, out string, err error) {
	f.responses[subCmd] = struct {
		out string
		err error
	}{out, err}
}

// ---- RandomDelay ----

func TestRandomDelay_WithinBounds(t *testing.T) {
	const iterations = 10_000
	for i := 0; i < iterations; i++ {
		v := RandomDelay(10, 50)
		if v < 10 || v > 50 {
			t.Fatalf("iteration %d: RandomDelay(10,50) = %d, want [10,50]", i, v)
		}
	}
}

func TestRandomDelay_EqualMinMax(t *testing.T) {
	for i := 0; i < 100; i++ {
		if v := RandomDelay(42, 42); v != 42 {
			t.Fatalf("RandomDelay(42,42) = %d, want 42", v)
		}
	}
}

func TestRandomDelay_MinGreaterThanMax(t *testing.T) {
	// When min > max, must return min without panicking.
	if v := RandomDelay(100, 10); v != 100 {
		t.Fatalf("RandomDelay(100,10) = %d, want 100 (clamp to min)", v)
	}
}

func TestRandomDelay_ZeroMin(t *testing.T) {
	for i := 0; i < 1000; i++ {
		v := RandomDelay(0, 5)
		if v < 0 || v > 5 {
			t.Fatalf("RandomDelay(0,5) = %d, out of range", v)
		}
	}
}

// ---- validateIface ----

func TestValidateIface_Valid(t *testing.T) {
	valid := []string{
		"eth0", "veth1a2b3c", "lo", "ovn-k8s-mp0",
		"some_iface", "iface.0",
	}
	for _, name := range valid {
		if err := validateIface(name); err != nil {
			t.Errorf("validateIface(%q) unexpected error: %v", name, err)
		}
	}
}

func TestValidateIface_Invalid(t *testing.T) {
	invalid := []string{
		"",
		"iface name",   // space
		"iface;drop",   // semicolon
		"iface$",       // dollar
		"../../etc",    // path traversal
		"iface\nmore",  // newline
		"iface`cmd`",   // backtick
	}
	for _, name := range invalid {
		if err := validateIface(name); err == nil {
			t.Errorf("validateIface(%q) expected error, got nil", name)
		}
	}
}

// ---- Apply ----

func TestApply_SuccessOnFirstReplace(t *testing.T) {
	f := newFakeRunner()
	defer f.install()()

	// "replace" succeeds immediately.
	f.setResponse("replace", "", nil)

	if err := Apply("veth0", 50); err != nil {
		t.Fatalf("Apply: unexpected error: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(f.calls))
	}
	assertArgContains(t, f.calls[0].args, "replace")
	assertArgContains(t, f.calls[0].args, "50ms")
}

func TestApply_FallsBackToAddWhenReplaceFails(t *testing.T) {
	f := newFakeRunner()
	defer f.install()()

	// "replace" fails (qdisc doesn't exist yet); "add" succeeds.
	f.setResponse("replace", "RTNETLINK error", errors.New("exit 2"))
	f.setResponse("add", "", nil)

	if err := Apply("veth0", 100); err != nil {
		t.Fatalf("Apply: unexpected error: %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("expected 2 calls (replace then add), got %d", len(f.calls))
	}
	assertArgContains(t, f.calls[1].args, "add")
}

func TestApply_BothReplaceAndAddFail(t *testing.T) {
	f := newFakeRunner()
	defer f.install()()

	f.setResponse("replace", "err", errors.New("replace failed"))
	f.setResponse("add", "err", errors.New("add failed"))

	if err := Apply("veth0", 50); err == nil {
		t.Fatal("Apply expected error when both replace and add fail")
	}
}

func TestApply_InvalidIface(t *testing.T) {
	f := newFakeRunner()
	defer f.install()()

	if err := Apply("bad iface", 50); err == nil {
		t.Fatal("Apply with invalid iface should return error")
	}
	if len(f.calls) != 0 {
		t.Fatal("runCmd should not be called for invalid iface")
	}
}

func TestApply_CommandContainsNetem(t *testing.T) {
	f := newFakeRunner()
	defer f.install()()

	f.setResponse("replace", "", nil)
	_ = Apply("eth0", 25)

	assertArgContains(t, f.calls[0].args, "netem")
}

// ---- Remove ----

func TestRemove_Success(t *testing.T) {
	f := newFakeRunner()
	defer f.install()()

	f.setResponse("del", "", nil)

	if err := Remove("veth0"); err != nil {
		t.Fatalf("Remove: unexpected error: %v", err)
	}
	assertArgContains(t, f.calls[0].args, "del")
}

func TestRemove_NoopWhenQdiscAbsent(t *testing.T) {
	f := newFakeRunner()
	defer f.install()()

	// tc prints "RTNETLINK answers: No such file or directory" when qdisc is missing.
	f.setResponse("del", "RTNETLINK answers: No such file or directory", errors.New("exit 2"))

	if err := Remove("veth0"); err != nil {
		t.Fatalf("Remove should be a no-op when qdisc is absent, got: %v", err)
	}
}

func TestRemove_ReturnsErrorOnUnexpectedFailure(t *testing.T) {
	f := newFakeRunner()
	defer f.install()()

	f.setResponse("del", "some unexpected error", errors.New("exit 1"))

	if err := Remove("veth0"); err == nil {
		t.Fatal("Remove expected error on unexpected failure")
	}
}

func TestRemove_InvalidIface(t *testing.T) {
	f := newFakeRunner()
	defer f.install()()

	if err := Remove(""); err == nil {
		t.Fatal("Remove with empty iface should return error")
	}
	if len(f.calls) != 0 {
		t.Fatal("runCmd should not be called for invalid iface")
	}
}

// ---- Show ----

func TestShow_ReturnsOutput(t *testing.T) {
	f := newFakeRunner()
	defer f.install()()

	expected := "qdisc netem 1: root refcnt 2 limit 1000 delay 50ms"
	f.setResponse("show", expected, nil)

	out, err := Show("veth0")
	if err != nil {
		t.Fatalf("Show: unexpected error: %v", err)
	}
	if out != expected {
		t.Errorf("Show output = %q, want %q", out, expected)
	}
}

func TestShow_InvalidIface(t *testing.T) {
	f := newFakeRunner()
	defer f.install()()

	if _, err := Show("bad;iface"); err == nil {
		t.Fatal("Show with invalid iface should return error")
	}
}

// ---- helpers ----

func assertArgContains(t *testing.T, args []string, want string) {
	t.Helper()
	for _, a := range args {
		if strings.Contains(a, want) || a == want {
			return
		}
	}
	t.Errorf("tc args %v do not contain %q", args, want)
}

func ExampleApply() {
	// Replace runCmd so this example works without a real tc binary.
	prev := runCmd
	runCmd = func(_ string, args ...string) (string, error) {
		fmt.Println("tc", strings.Join(args, " "))
		return "", nil
	}
	defer func() { runCmd = prev }()

	_ = Apply("veth0abc", 50)
	// Output:
	// tc qdisc replace dev veth0abc root handle 1: netem delay 50ms
}
