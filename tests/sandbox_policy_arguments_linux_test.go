//go:build linux && sandbox_root && profilebundle_root

package workspace_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestSandboxExecutionSecurityComparesFullWidthPolicyArguments(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("System Call Policy argument tests require root in a disposable Linux environment")
	}
	components := readOuterBundle(t, os.Getenv("SANDBOX_EXECUTION_BUNDLE"))
	for _, name := range []string{"profile.lock.json", "system-call-policy.json", "rootfs.tar", "sandbox-init"} {
		if len(components[name]) == 0 {
			t.Fatalf("real Python Profile Bundle has no %s", name)
		}
	}
	// Expectations are explicit observations of unsigned 64-bit comparisons,
	// including values whose high bit would reverse a signed comparison.
	tests := []struct {
		operator string
		value    uint64
		mask     uint64
		probes   []policyArgumentProbe
	}{
		{
			operator: "equal", value: 0x8000000100000002,
			probes: []policyArgumentProbe{
				{0x8000000100000002, true},
				{0x8000000100000001, false},
				{0x8000000100000003, false},
				{0x0000000100000002, false},
				{0x8000000000000002, false},
				{0xffffffffffffffff, false},
				{0, false},
			},
		},
		{
			operator: "not-equal", value: 0x8000000100000002,
			probes: []policyArgumentProbe{
				{0x8000000100000002, false},
				{0x8000000100000001, true},
				{0x8000000100000003, true},
				{0x0000000100000002, true},
				{0x8000000000000002, true},
				{0xffffffffffffffff, true},
				{0, true},
			},
		},
		{
			operator: "less-than", value: 0x8000000100000002,
			probes: []policyArgumentProbe{
				{0, true},
				{0x0000000100000002, true},
				{0x7fffffffffffffff, true},
				{0x80000000ffffffff, true},
				{0x8000000100000001, true},
				{0x8000000100000002, false},
				{0x8000000100000003, false},
				{0x8000000200000000, false},
				{0xffffffffffffffff, false},
			},
		},
		{
			operator: "less-or-equal", value: 0x8000000100000002,
			probes: []policyArgumentProbe{
				{0, true},
				{0x0000000100000002, true},
				{0x7fffffffffffffff, true},
				{0x80000000ffffffff, true},
				{0x8000000100000001, true},
				{0x8000000100000002, true},
				{0x8000000100000003, false},
				{0x8000000200000000, false},
				{0xffffffffffffffff, false},
			},
		},
		{
			operator: "greater-than", value: 0x8000000100000002,
			probes: []policyArgumentProbe{
				{0, false},
				{0x0000000100000002, false},
				{0x7fffffffffffffff, false},
				{0x80000000ffffffff, false},
				{0x8000000100000001, false},
				{0x8000000100000002, false},
				{0x8000000100000003, true},
				{0x8000000200000000, true},
				{0xffffffffffffffff, true},
			},
		},
		{
			operator: "greater-or-equal", value: 0x8000000100000002,
			probes: []policyArgumentProbe{
				{0, false},
				{0x0000000100000002, false},
				{0x7fffffffffffffff, false},
				{0x80000000ffffffff, false},
				{0x8000000100000001, false},
				{0x8000000100000002, true},
				{0x8000000100000003, true},
				{0x8000000200000000, true},
				{0xffffffffffffffff, true},
			},
		},
		{
			operator: "masked-equal", value: 0x8000000100000002, mask: 0xffffffff0000ffff,
			probes: []policyArgumentProbe{
				{0x8000000100000002, true},
				{0x80000001ffff0002, true},
				{0x8000000112340002, true},
				{0x0000000100000002, false},
				{0x8000000200000002, false},
				{0x8000000100000001, false},
				{0x8000000100000003, false},
				{0xffffffffffffffff, false},
				{0, false},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.operator, func(t *testing.T) {
			argument := independentPolicyArgument{Index: 5, Operator: test.operator, Value: test.value, Mask: test.mask}
			fixture, identity := installedPolicyArgumentFixture(t, components, argument)
			t.Cleanup(startSandboxSupervisor(t, fixture,
				"--subuid-start", "200000", "--subgid-start", "300000", "--subid-count", "65536",
				"--cgroup-root", os.Getenv("SANDBOX_CGROUP_ROOT")))
			assertSandboxCreated(t, exchangeSandboxSupervisorMessage(t, fixture.socketPath,
				createSandboxWireRequest(t, "create", "run-arguments", identity)), "run-arguments")
			result := executeSandboxPython(t, fixture, "run-arguments", "probe", policyArgumentProbeSource(test.probes))
			if result.ExitCode != 0 || result.Stdout != "argument-predicates-ok\n" {
				t.Fatalf("%s failed through the installed Profile and Supervisor: %+v", test.operator, result)
			}
		})
	}
}

type policyArgumentProbe struct {
	value   uint64
	allowed bool
}

func policyArgumentProbeSource(probes []policyArgumentProbe) string {
	var source strings.Builder
	source.WriteString("import ctypes, errno\nlibc = ctypes.CDLL(None, use_errno=True)\nlibc.syscall.restype = ctypes.c_long\nprobes = [\n")
	for _, probe := range probes {
		allowed := "False"
		if probe.allowed {
			allowed = "True"
		}
		fmt.Fprintf(&source, "    (%d, %s),\n", probe.value, allowed)
	}
	// Native getpriority uses only its first two arguments. Its sixth syscall
	// register is therefore a harmless carrier for every possible uint64.
	source.WriteString(`]
for value, allowed in probes:
    ctypes.set_errno(0)
    result = libc.syscall(140, 0, 0, 0, 0, 0, ctypes.c_uint64(value))
    error = ctypes.get_errno()
    if allowed:
        assert result >= 0 and error == 0, (hex(value), result, error, 'expected allowed')
    else:
        assert result == -1 and error == errno.EPERM, (hex(value), result, error, 'expected denied')
print('argument-predicates-ok')
`)
	return source.String()
}

// These wire-format types are independent of the runtime's parser/compiler.
// Preserve all production rules and change only the getpriority probe rule.
type independentArgumentPolicy struct {
	Schema        string                  `json:"schema"`
	Profile       string                  `json:"profile"`
	Target        json.RawMessage         `json:"target"`
	DefaultAction json.RawMessage         `json:"default_action"`
	Rules         []independentPolicyRule `json:"rules"`
}

type independentPolicyRule struct {
	Names     []string                    `json:"names"`
	Action    string                      `json:"action"`
	Arguments []independentPolicyArgument `json:"arguments"`
}

type independentPolicyArgument struct {
	Index    uint   `json:"index"`
	Operator string `json:"operator"`
	Value    uint64 `json:"value"`
	Mask     uint64 `json:"mask"`
}

func installedPolicyArgumentFixture(t *testing.T, components map[string][]byte, argument independentPolicyArgument) (sandboxSupervisorFixture, string) {
	t.Helper()
	var policy independentArgumentPolicy
	if err := json.Unmarshal(components["system-call-policy.json"], &policy); err != nil {
		t.Fatalf("read real Profile System Call Policy independently: %v", err)
	}
	rules := make([]independentPolicyRule, 0)
	for _, rule := range policy.Rules {
		for _, name := range rule.Names {
			if name == "getpriority" {
				t.Fatal("production policy unexpectedly already permits getpriority")
			}
			rules = append(rules, independentPolicyRule{Names: []string{name}, Action: rule.Action, Arguments: rule.Arguments})
		}
	}
	rules = append(rules, independentPolicyRule{Names: []string{"getpriority"}, Action: "allow", Arguments: []independentPolicyArgument{argument}})
	sort.Slice(rules, func(left, right int) bool { return rules[left].Names[0] < rules[right].Names[0] })
	policy.Rules = rules
	encoded, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "argument-policy.bundle")
	digest := writeIndependentBundleFixtureWithRootFSAndInit(t, bundle,
		components["profile.lock.json"], append(encoded, '\n'), components["rootfs.tar"], components["sandbox-init"])
	fixture := newSafeSandboxSupervisorFixture(t)
	if err := os.Chmod(filepath.Join(fixture.profileStore, "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	install := exec.Command(os.Getenv("PROFILE_BUNDLE_CLI"), "install", "--bundle", bundle,
		"--expected-sha256", digest, "--store", fixture.profileStore)
	if output, err := install.CombinedOutput(); err != nil {
		t.Fatalf("install independent argument System Call Policy Bundle: %v\n%s", err, output)
	}
	return fixture, "sha256:" + digest
}
