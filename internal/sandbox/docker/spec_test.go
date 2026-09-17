package docker

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
	"github.com/austmsk/conclave/internal/sandbox/sandboxtest"
)

const digestOnlyImage = "ghcr.io/austmsk/sandbox-go@sha256:0000000000000000000000000000000000000000000000000000000000000000"

func validSpec(t *testing.T) contracts.SandboxSpec {
	t.Helper()
	tree, _ := sandboxtest.NewRepo(t)
	if err := sandbox.PrepareTree(tree); err != nil {
		t.Fatal(err)
	}
	return contracts.SandboxSpec{
		Image:    digestOnlyImage,
		TreePath: tree,
		Env:      map[string]string{"GOFLAGS": "-mod=mod"},
		Network:  contracts.NetworkNone,
		Limits: contracts.ResourceLimits{
			MemoryBytes: 256 << 20,
			CPUShares:   512,
			DiskBytes:   64 << 20,
			MaxProcs:    64,
			Timeout:     time.Minute,
		},
	}
}

func TestValidateSpecRejectsUnsafeSpecs(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(s *contracts.SandboxSpec)
		want   error
	}{
		{"image by tag", func(s *contracts.SandboxSpec) { s.Image = "alpine:3.22" }, ErrInvalidSpec},
		{"image by latest", func(s *contracts.SandboxSpec) { s.Image = "alpine" }, ErrInvalidSpec},
		{"image by short digest", func(s *contracts.SandboxSpec) { s.Image = "alpine@sha256:abcd" }, ErrInvalidSpec},
		{"unknown network", func(s *contracts.SandboxSpec) { s.Network = "host" }, ErrInvalidSpec},
		{"empty network", func(s *contracts.SandboxSpec) { s.Network = "" }, ErrInvalidSpec},
		{"zero memory", func(s *contracts.SandboxSpec) { s.Limits.MemoryBytes = 0 }, ErrInvalidSpec},
		{"zero cpu", func(s *contracts.SandboxSpec) { s.Limits.CPUShares = 0 }, ErrInvalidSpec},
		{"zero disk", func(s *contracts.SandboxSpec) { s.Limits.DiskBytes = 0 }, ErrInvalidSpec},
		{"tiny disk", func(s *contracts.SandboxSpec) { s.Limits.DiskBytes = 1 << 20 }, ErrInvalidSpec},
		{"disk equal to memory", func(s *contracts.SandboxSpec) { s.Limits.DiskBytes = s.Limits.MemoryBytes }, ErrInvalidSpec},
		{"disk above memory", func(s *contracts.SandboxSpec) { s.Limits.DiskBytes = s.Limits.MemoryBytes + 1 }, ErrInvalidSpec},
		{"zero procs", func(s *contracts.SandboxSpec) { s.Limits.MaxProcs = 0 }, ErrInvalidSpec},
		{"too few procs", func(s *contracts.SandboxSpec) { s.Limits.MaxProcs = 4 }, ErrInvalidSpec},
		{"zero timeout", func(s *contracts.SandboxSpec) { s.Limits.Timeout = 0 }, ErrInvalidSpec},
		{"negative timeout", func(s *contracts.SandboxSpec) { s.Limits.Timeout = -time.Second }, ErrInvalidSpec},
		{"env overrides HOME", func(s *contracts.SandboxSpec) { s.Env["HOME"] = "/" }, ErrInvalidSpec},
		{"env overrides CI", func(s *contracts.SandboxSpec) { s.Env["CI"] = "false" }, ErrInvalidSpec},
		{"env key with equals", func(s *contracts.SandboxSpec) { s.Env["A=B"] = "x" }, ErrInvalidSpec},
		{"env key with space", func(s *contracts.SandboxSpec) { s.Env["A B"] = "x" }, ErrInvalidSpec},
		{"empty tree path", func(s *contracts.SandboxSpec) { s.TreePath = "" }, ErrInvalidSpec},
		{"missing tree", func(s *contracts.SandboxSpec) { s.TreePath = t.TempDir() }, sandbox.ErrTreeNotPrepared},
		{"unprepared tree", func(s *contracts.SandboxSpec) { s.TreePath, _ = sandboxtest.NewRepo(t) }, sandbox.ErrTreeNotPrepared},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := validSpec(t)
			tc.mutate(&spec)
			err := validateSpec(spec)
			if !errors.Is(err, tc.want) {
				t.Fatalf("validateSpec = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestValidateSpecAcceptsAValidSpec(t *testing.T) {
	if err := validateSpec(validSpec(t)); err != nil {
		t.Fatal(err)
	}
}

func TestBuildEnvIsFixedPlusAllowlistOnly(t *testing.T) {
	t.Setenv("CONCLAVE_WORKER_SECRET", "canary_worker_secret")
	got := buildEnv(map[string]string{"ZZZ": "last", "AAA": "first", "GOFLAGS": "-mod=mod"})
	want := []string{"HOME=/home/agent", "TMPDIR=/tmp", "CI=true", "USER=agent", "AAA=first", "GOFLAGS=-mod=mod", "ZZZ=last"}
	if !slices.Equal(got, want) {
		t.Fatalf("buildEnv = %v, want %v", got, want)
	}
}

func TestDiskSplitSumsToDiskBytes(t *testing.T) {
	for _, disk := range []int64{8 << 20, 100 << 20, 1 << 30, (1 << 30) + 12345} {
		w, h, tmp := diskSplit(disk)
		if w+h+tmp != disk {
			t.Errorf("diskSplit(%d) = %d+%d+%d != %d", disk, w, h, tmp, disk)
		}
		if w <= 0 || h <= 0 || tmp <= 0 {
			t.Errorf("diskSplit(%d) produced an empty mount: %d %d %d", disk, w, h, tmp)
		}
	}
}

func TestHostConfigIsHardened(t *testing.T) {
	p := &Provider{cfg: Config{}.withDefaults()}
	spec := validSpec(t)
	hc := p.hostConfig(spec, "none", []string{"/data", "/workspace"})

	checks := []struct {
		name string
		ok   bool
	}{
		{"root filesystem read-only", hc.ReadonlyRootfs},
		{"not privileged", !hc.Privileged},
		{"all capabilities dropped", slices.Equal(hc.CapDrop, []string{"ALL"}) && len(hc.CapAdd) == 0},
		{"no-new-privileges", slices.Contains(hc.SecurityOpt, "no-new-privileges:true")},
		{"no binds or mounts", len(hc.Binds) == 0 && len(hc.Mounts) == 0},
		{"pids limit", hc.PidsLimit != nil && *hc.PidsLimit == spec.Limits.MaxProcs},
		{"memory and swap capped", hc.Memory == spec.Limits.MemoryBytes && hc.MemorySwap == spec.Limits.MemoryBytes},
		{"network none", hc.NetworkMode == "none"},
		{"private ipc", hc.IpcMode == container.IPCModePrivate},
		{"image volume covered read-only", hc.Tmpfs["/data"] == "size=4k,mode=0755,ro"},
		{"writable mounts allow exec without suid", strings.Contains(hc.Tmpfs[workspaceDir], ",exec,nosuid,nodev") && strings.Contains(hc.Tmpfs[tmpDir], ",exec,") && strings.Contains(hc.Tmpfs[homeDir], ",exec,")},
		{"workspace mount wins over an image volume", strings.HasPrefix(hc.Tmpfs[workspaceDir], "size="+strconv.FormatInt(spec.Limits.DiskBytes/2, 10)+",")},
		{"home and tmp mounted", hc.Tmpfs[homeDir] != "" && hc.Tmpfs[tmpDir] != ""},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s: violated in %+v", c.name, hc)
		}
	}
}

func TestContainerConfigRunsAsSandboxUser(t *testing.T) {
	p := &Provider{cfg: Config{}.withDefaults()}
	cc := p.containerConfig(validSpec(t))
	if cc.User != "1000:1000" {
		t.Errorf("user = %q, want 1000:1000", cc.User)
	}
	if cc.Labels[labelSandbox] != "true" || cc.Labels[labelNamespace] != "default" {
		t.Errorf("labels = %v", cc.Labels)
	}
	if !slices.Equal(cc.Entrypoint, []string{"/bin/sh"}) || cc.WorkingDir != workspaceDir {
		t.Errorf("entrypoint=%v workdir=%q", cc.Entrypoint, cc.WorkingDir)
	}
}
