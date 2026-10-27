package sandbox

import (
	"fmt"
	"regexp"

	"github.com/austmsk/conclave/internal/contracts"
)

// Rules every backend applies to a contracts.SandboxSpec, kept here so the
// startup configuration (LoadConfig) refuses what a provider's Create
// would, from one source of truth.

const (
	// MinDiskBytes is the smallest DiskBytes a backend accepts: the disk is
	// split among several writable mounts, and each must be usable.
	MinDiskBytes = 8 << 20

	// MinProcs is the smallest MaxProcs a backend accepts: the sandbox's
	// own init holds some pids, and the agent needs some of its own.
	MinProcs = 8
)

var (
	// digestImage accepts only references pinned by digest (FR-SEC30).
	digestImage = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]*@sha256:[0-9a-f]{64}$`)

	envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

	// reservedEnv is set by the backend and may not be overridden, because
	// the values point at the sandbox's writable mounts.
	reservedEnv = map[string]bool{"HOME": true, "TMPDIR": true, "CI": true, "USER": true}
)

// DigestPinned reports whether image is a reference pinned by a full
// sha256 digest (FR-SEC30). A tag, with or without a digest-looking
// suffix, is not: what runs must be exactly what was reviewed.
func DigestPinned(image string) bool {
	return digestImage.MatchString(image)
}

// ValidEnvKey reports whether key is a shell identifier, the only form a
// sandbox environment variable name may take.
func ValidEnvKey(key string) bool {
	return envKey.MatchString(key)
}

// ReservedEnv reports whether name is one the backend sets itself (HOME,
// TMPDIR, CI, USER), which a spec may not override.
func ReservedEnv(name string) bool {
	return reservedEnv[name]
}

// CheckLimits applies the rules every backend enforces on resource limits:
// every limit positive, DiskBytes at least MinDiskBytes and below
// MemoryBytes, MaxProcs at least MinProcs. The error names the field.
func CheckLimits(l contracts.ResourceLimits) error {
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
			return fmt.Errorf("limit %s must be positive", f.name)
		}
	}
	if l.DiskBytes < MinDiskBytes {
		return fmt.Errorf("DiskBytes below %d", MinDiskBytes)
	}
	// The disk is tmpfs, and tmpfs pages are charged to the memory cgroup.
	// A disk the memory limit cannot hold turns a full disk into an OOM
	// kill at an arbitrary point instead of a clean ENOSPC.
	if l.DiskBytes >= l.MemoryBytes {
		return fmt.Errorf("DiskBytes (%d) must be below MemoryBytes (%d): the disk is memory-backed", l.DiskBytes, l.MemoryBytes)
	}
	if l.MaxProcs < MinProcs {
		return fmt.Errorf("MaxProcs below %d", MinProcs)
	}
	return nil
}
