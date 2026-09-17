package docker

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"

	"github.com/moby/moby/api/types/container"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
)

// ErrInvalidSpec reports a SandboxSpec the provider refuses to run.
var ErrInvalidSpec = errors.New("invalid sandbox spec")

const (
	workspaceDir = "/workspace"
	homeDir      = "/home/agent"
	tmpDir       = "/tmp"

	labelSandbox   = "conclave.sandbox"
	labelNamespace = "conclave.sandbox.namespace"

	// shmBytes bounds /dev/shm, which Docker mounts writable even under a
	// read-only root filesystem. It is the only writable space outside the
	// DiskBytes accounting.
	shmBytes = 8 << 20

	// minDiskBytes keeps every tmpfs split non-trivial.
	minDiskBytes = 8 << 20

	// initScript is PID 1. See the package documentation for why it, and
	// not the provider, kills runaway processes.
	initScript = `trap 'i=0; while [ $i -lt 64 ]; do kill -9 -1 2>/dev/null; i=$((i+1)); done' USR1
while :; do sleep 3600 & wait $!; done`
)

var (
	// digestImage accepts only references pinned by digest (FR-SEC30).
	digestImage = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]*@sha256:[0-9a-f]{64}$`)
	envKey      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

	// reservedEnv is set by the provider and may not be overridden by a
	// spec, because the values point at the writable mounts.
	reservedEnv = map[string]bool{"HOME": true, "TMPDIR": true, "CI": true, "USER": true}
)

func validateSpec(spec contracts.SandboxSpec) error {
	if !digestImage.MatchString(spec.Image) {
		return fmt.Errorf("%w: image %q is not pinned by digest", ErrInvalidSpec, spec.Image)
	}
	if spec.Network != contracts.NetworkNone && spec.Network != contracts.NetworkRegistries {
		return fmt.Errorf("%w: unknown network mode %q", ErrInvalidSpec, spec.Network)
	}
	if err := validateLimits(spec.Limits); err != nil {
		return err
	}
	if err := validateEnv(spec.Env); err != nil {
		return err
	}
	if spec.TreePath == "" {
		return fmt.Errorf("%w: empty tree path", ErrInvalidSpec)
	}
	if err := sandbox.VerifyTree(spec.TreePath); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSpec, err)
	}
	return nil
}

func validateLimits(l contracts.ResourceLimits) error {
	fields := []struct {
		name  string
		value int64
	}{
		{"MemoryBytes", l.MemoryBytes},
		{"CPUShares", l.CPUShares},
		{"DiskBytes", l.DiskBytes},
		{"MaxProcs", l.MaxProcs},
		{"Timeout", int64(l.Timeout)},
	}
	for _, f := range fields {
		if f.value <= 0 {
			return fmt.Errorf("%w: limit %s must be positive", ErrInvalidSpec, f.name)
		}
	}
	if l.DiskBytes < minDiskBytes {
		return fmt.Errorf("%w: DiskBytes below %d", ErrInvalidSpec, minDiskBytes)
	}
	// The disk is tmpfs, and tmpfs pages are charged to the memory cgroup.
	// A disk the memory limit cannot hold turns a full disk into an OOM
	// kill at an arbitrary point instead of a clean ENOSPC.
	if l.DiskBytes >= l.MemoryBytes {
		return fmt.Errorf("%w: DiskBytes (%d) must be below MemoryBytes (%d): the disk is memory-backed", ErrInvalidSpec, l.DiskBytes, l.MemoryBytes)
	}
	// The init loop holds two pids; the agent needs some of its own.
	if l.MaxProcs < 8 {
		return fmt.Errorf("%w: MaxProcs below 8", ErrInvalidSpec)
	}
	return nil
}

func validateEnv(env map[string]string) error {
	for k := range env {
		if !envKey.MatchString(k) {
			return fmt.Errorf("%w: env key %q is not an identifier", ErrInvalidSpec, k)
		}
		if reservedEnv[k] {
			return fmt.Errorf("%w: env key %s is set by the provider", ErrInvalidSpec, k)
		}
	}
	return nil
}

// buildEnv is the whole environment the container sees: the provider's
// fixed values and the spec's allowlist, in key order. Nothing is read
// from the worker's own environment (FR-SEC23).
func buildEnv(env map[string]string) []string {
	out := []string{"HOME=" + homeDir, "TMPDIR=" + tmpDir, "CI=true", "USER=agent"}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// diskSplit divides DiskBytes among the three writable mounts. The
// workspace gets half; home takes most of the rest because toolchain
// caches live there.
func diskSplit(disk int64) (workspace, home, tmp int64) {
	workspace = disk / 2
	home = disk * 3 / 8
	tmp = disk - workspace - home
	return workspace, home, tmp
}

// tmpfsOption renders a writable mount. Docker adds noexec to every tmpfs
// unless told otherwise; a build-and-test sandbox has to run what it
// builds, so exec is requested explicitly (docs/decisions/0004). nosuid
// and nodev stay: no capability is available to make either matter, and
// nothing legitimate needs them.
func tmpfsOption(size int64, mode string, uid, gid int) string {
	return "size=" + strconv.FormatInt(size, 10) + ",mode=" + mode +
		",uid=" + strconv.Itoa(uid) + ",gid=" + strconv.Itoa(gid) + ",exec,nosuid,nodev"
}

func (p *Provider) containerConfig(spec contracts.SandboxSpec) *container.Config {
	return &container.Config{
		Image:      spec.Image,
		User:       strconv.Itoa(p.cfg.UID) + ":" + strconv.Itoa(p.cfg.GID),
		Entrypoint: []string{"/bin/sh"},
		Cmd:        []string{"-c", initScript},
		WorkingDir: workspaceDir,
		Env:        buildEnv(spec.Env),
		Hostname:   "sandbox",
		Labels:     p.labels(),
	}
}

// hostConfig builds the hardened container settings. Every path the image
// declares as a VOLUME is covered with a tiny read-only tmpfs; otherwise
// the daemon would create an anonymous volume there, which is writable host
// storage outside both the quota and the read-only root filesystem.
func (p *Provider) hostConfig(spec contracts.SandboxSpec, networkMode string, imageVolumes []string) *container.HostConfig {
	workspace, home, tmp := diskSplit(spec.Limits.DiskBytes)
	pids := spec.Limits.MaxProcs
	uid, gid := p.cfg.UID, p.cfg.GID
	tmpfs := make(map[string]string, len(imageVolumes)+3)
	for _, path := range imageVolumes {
		tmpfs[path] = "size=4k,mode=0755,ro"
	}
	tmpfs[workspaceDir] = tmpfsOption(workspace, "0755", uid, gid)
	tmpfs[homeDir] = tmpfsOption(home, "0700", uid, gid)
	tmpfs[tmpDir] = tmpfsOption(tmp, "1777", 0, 0)
	return &container.HostConfig{
		Resources: container.Resources{
			Memory:     spec.Limits.MemoryBytes,
			MemorySwap: spec.Limits.MemoryBytes,
			CPUShares:  spec.Limits.CPUShares,
			PidsLimit:  &pids,
		},
		NetworkMode:    container.NetworkMode(networkMode),
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges:true"},
		ReadonlyRootfs: true,
		Privileged:     false,
		IpcMode:        container.IPCModePrivate,
		ShmSize:        shmBytes,
		Tmpfs:          tmpfs,
	}
}

func (p *Provider) labels() map[string]string {
	return map[string]string{labelSandbox: "true", labelNamespace: p.cfg.Namespace}
}
