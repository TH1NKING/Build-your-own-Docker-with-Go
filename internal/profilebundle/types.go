// Package profilebundle builds and installs immutable Runtime Profile bundles.
package profilebundle

type BuildOptions struct {
	LockPath             string
	SourceCache          string
	SystemCallPolicyPath string
	SandboxInitPath      string
	OutputPath           string
}

type InstallOptions struct {
	BundlePath     string
	ExpectedSHA256 string
	StorePath      string
}

type buildLock struct {
	Schema  string        `json:"schema"`
	Profile string        `json:"profile"`
	Target  target        `json:"target"`
	Python  pythonRuntime `json:"python"`
	Inputs  []lockedInput `json:"inputs"`
}

type target struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

type pythonRuntime struct {
	Version    string          `json:"version"`
	Entrypoint string          `json:"entrypoint"`
	Packages   []lockedPackage `json:"packages"`
}

type lockedPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

type lockedInput struct {
	ID            string   `json:"id"`
	Kind          string   `json:"kind"`
	Version       string   `json:"version"`
	Source        string   `json:"source"`
	Filename      string   `json:"filename"`
	Size          int64    `json:"size"`
	Digest        string   `json:"digest"`
	ArchivePrefix string   `json:"archive_prefix"`
	Destination   string   `json:"destination"`
	Include       []string `json:"include"`
	Exclude       []string `json:"exclude"`
}

type systemCallPolicy struct {
	Schema        string                 `json:"schema"`
	Profile       string                 `json:"profile"`
	Target        target                 `json:"target"`
	DefaultAction policyDefaultAction    `json:"default_action"`
	Rules         []systemCallPolicyRule `json:"rules"`
}

type policyDefaultAction struct {
	Action string `json:"action"`
	Errno  int    `json:"errno"`
}

type systemCallPolicyRule struct {
	Names     []string                   `json:"names"`
	Action    string                     `json:"action"`
	Arguments []systemCallPolicyArgument `json:"arguments"`
}

type systemCallPolicyArgument struct {
	Index    uint   `json:"index"`
	Operator string `json:"operator"`
	Value    uint64 `json:"value"`
	Mask     uint64 `json:"mask"`
}

type manifest struct {
	Schema     string        `json:"schema"`
	Profile    string        `json:"profile"`
	Target     target        `json:"target"`
	Python     pythonRuntime `json:"python"`
	Components []component   `json:"components"`
}

type component struct {
	Role   string `json:"role"`
	Path   string `json:"path"`
	Format string `json:"format"`
	Mode   string `json:"mode"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

var bundleV1Components = []component{
	{Role: "build-lock", Path: "profile.lock.json", Format: "profile-build-lock/v1", Mode: "0444"},
	{Role: "system-call-policy", Path: "system-call-policy.json", Format: "system-call-policy/v1", Mode: "0444"},
	{Role: "sandbox-init", Path: "sandbox-init", Format: "opaque/v1", Mode: "0555"},
	{Role: "rootfs", Path: "rootfs.tar", Format: "rootfs-pax-tar/v1", Mode: "0444"},
}
