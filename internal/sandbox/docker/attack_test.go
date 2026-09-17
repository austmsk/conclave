package docker_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox/docker"
	"github.com/austmsk/conclave/internal/sandbox/sandboxtest"
)

// These tests attempt the attack each control exists to stop
// (docs/security-and-threat-model.md, "Sandbox attack tests").

// probe reports a TCP connect, a DNS lookup, the routing table (header
// line only when there is no route out) and the interface list, so a
// resolver-only or interface-only leak would also show. The kernel's
// tunnel stubs (gre0, sit0, ...) appear in every namespace and are not
// interfaces out.
const probe = `nc -z -w 2 1.1.1.1 53; echo tcp=$?; nslookup example.com >/dev/null 2>&1; echo dns=$?; echo routes=$(wc -l < /proc/net/route); echo eth=$(ls /sys/class/net | grep -c '^eth')`

// cut is what probe prints when the container has only loopback.
const cut = "tcp=1\ndns=1\nroutes=1\neth=0"

func TestAttackOutboundNetworkFailsWithNetworkNone(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, h.spec(t, contracts.NetworkNone))
	if got := h.out(t, sb, probe); got != cut {
		t.Errorf("network reachable with NetworkNone:\n%s", got)
	}
	assertNoNetworks(t, h.inspect(t, sb).NetworkSettings.Networks)
}

func TestAttackRegistriesIsRefusedThroughCreate(t *testing.T) {
	h := newHarness(t)
	_, err := h.p.Create(h.ctx, h.spec(t, contracts.NetworkRegistries))
	if !errors.Is(err, docker.ErrInvalidSpec) {
		t.Fatalf("Create with NetworkRegistries = %v, want ErrInvalidSpec", err)
	}
	if n := len(h.containers(t)); n != 0 {
		t.Errorf("%d containers created for a refused spec", n)
	}
}

func TestAttackOutboundNetworkFailsAfterInstallPhase(t *testing.T) {
	h := newHarness(t)
	raw, results, err := h.p.CreateWithInstall(h.ctx, h.spec(t, contracts.NetworkRegistries), []contracts.ExecRequest{
		{Command: []string{"/bin/sh", "-c", "ls /sys/class/net; echo routes=$(wc -l < /proc/net/route)"}},
	})
	if err != nil {
		t.Fatalf("CreateWithInstall: %v", err)
	}
	sb := raw.(*docker.Sandbox)
	t.Cleanup(func() { _ = sb.Close(h.ctx) })
	if len(results) != 1 || !strings.Contains(string(results[0].Stdout), "eth0") || strings.Contains(string(results[0].Stdout), "routes=1\n") {
		t.Fatalf("install phase should have had an interface and a route: %+v", results)
	}
	if got := h.out(t, sb, probe); got != cut {
		t.Errorf("network reachable after the install phase:\n%s", got)
	}
	assertNoNetworks(t, h.inspect(t, sb).NetworkSettings.Networks)
	if err := sb.Close(h.ctx); err != nil {
		t.Fatal(err)
	}
	if n := h.networks(t); n != 0 {
		t.Errorf("%d networks remain after Close", n)
	}
}

func TestAttackFailedInstallNeverHandsOutANetworkedSandbox(t *testing.T) {
	h := newHarness(t)
	sb, results, err := h.p.CreateWithInstall(h.ctx, h.spec(t, contracts.NetworkRegistries), []contracts.ExecRequest{
		{Command: []string{"true"}},
		{Command: []string{"/bin/sh", "-c", "echo broken >&2; exit 7"}},
		{Command: []string{"echo", "never"}},
	})
	if !errors.Is(err, docker.ErrInstallFailed) || sb != nil {
		t.Fatalf("CreateWithInstall = %v, %v; want ErrInstallFailed and no sandbox", sb, err)
	}
	if len(results) != 2 || results[1].ExitCode != 7 || !strings.Contains(string(results[1].Stderr), "broken") {
		t.Errorf("results = %+v", results)
	}
	if n := len(h.containers(t)); n != 0 {
		t.Errorf("%d containers remain after a failed install", n)
	}
	if n := h.networks(t); n != 0 {
		t.Errorf("%d networks remain after a failed install", n)
	}
}

// assertNoNetworks allows only Docker's placeholder "none" entry.
func assertNoNetworks(t *testing.T, nets map[string]*network.EndpointSettings) {
	t.Helper()
	for name := range nets {
		if name != "none" {
			t.Errorf("container attached to network %q", name)
		}
	}
}

func TestAttackHostPathsUnreadable(t *testing.T) {
	h := newHarness(t)
	spec := h.spec(t, contracts.NetworkNone)
	sb := h.create(t, spec)

	secret := filepath.Join(t.TempDir(), "worker-secret")
	if err := os.WriteFile(secret, []byte("canary_host_file"), 0o644); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	for _, path := range []string{secret, home, "/Users", "/var/lib/docker", "/var/run/docker.sock", "/run/docker.sock"} {
		res := h.sh(t, sb, "cat "+strconv.Quote(path)+" 2>&1 || ls "+strconv.Quote(path)+" 2>&1")
		if res.ExitCode == 0 || strings.Contains(string(res.Stdout), "canary_host_file") {
			t.Errorf("host path %s reachable: exit=%d out=%q", path, res.ExitCode, res.Stdout)
		}
	}
	// The tree was copied, not mounted: the worker's copy is not the same
	// file, so an in-sandbox write cannot alter it.
	h.out(t, sb, "echo tampered > README.md")
	if data, _ := os.ReadFile(filepath.Join(spec.TreePath, "README.md")); strings.Contains(string(data), "tampered") {
		t.Error("sandbox write reached the worker's tree")
	}

	// The test image declares VOLUME /git; without the tmpfs cover the
	// daemon would give it an anonymous host volume.
	insp := h.inspect(t, sb)
	if len(insp.HostConfig.Binds) != 0 {
		t.Errorf("binds present: %v", insp.HostConfig.Binds)
	}
	for _, m := range insp.Mounts {
		t.Errorf("mount %s (%s) at %s: storage outside the sandbox", m.Source, m.Type, m.Destination)
	}
	if got := h.sh(t, sb, "touch /git/x 2>&1; echo exit=$?"); !strings.Contains(string(got.Stdout), "exit=1") {
		t.Errorf("image-declared volume is writable: %s", got.Stdout)
	}
}

func TestAttackProcessIsNonRootWithoutCapabilities(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, h.spec(t, contracts.NetworkNone))

	got := h.out(t, sb, `id -u; id -g; grep -E '^(CapEff|CapPrm|CapBnd|NoNewPrivs)' /proc/self/status | tr -s '\t' ' '`)
	want := "1000\n1000\nCapPrm: 0000000000000000\nCapEff: 0000000000000000\nCapBnd: 0000000000000000\nNoNewPrivs: 1"
	if got != want {
		t.Errorf("identity and capabilities:\n%s\nwant:\n%s", got, want)
	}
	res := h.sh(t, sb, "touch /etc/pwned 2>&1; echo etc=$?; touch /usr/bin/pwned 2>&1; echo usr=$?; su -c id root </dev/null 2>&1; echo su=$?")
	out := string(res.Stdout)
	if !strings.Contains(out, "etc=1") || !strings.Contains(out, "usr=1") || !strings.Contains(out, "su=1") {
		t.Errorf("privilege gained:\n%s", out)
	}
	if strings.Contains(out, "uid=0") {
		t.Errorf("root reached:\n%s", out)
	}
	if insp := h.inspect(t, sb); insp.HostConfig.Privileged || !insp.HostConfig.ReadonlyRootfs {
		t.Error("container is privileged or root filesystem writable")
	}
}

func TestAttackDockerSocketAbsent(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, h.spec(t, contracts.NetworkNone))

	got := h.out(t, sb, `for s in /var/run/docker.sock /run/docker.sock; do [ -e "$s" ] && echo "present $s"; done; ls /var/run /run 2>/dev/null | grep -c docker || true; env | grep -c DOCKER || true`)
	if strings.Contains(got, "present") || got != "0\n0" {
		t.Errorf("a docker socket or DOCKER_* variable is reachable:\n%s", got)
	}
	res := h.sh(t, sb, "nc -U -w 1 /var/run/docker.sock </dev/null 2>&1; echo exit=$?")
	if !strings.Contains(string(res.Stdout), "exit=1") {
		t.Errorf("connecting to the docker socket did not fail: %s", res.Stdout)
	}
}

func TestAttackForkBombIsContained(t *testing.T) {
	h := newHarness(t)
	spec := h.spec(t, contracts.NetworkNone)
	spec.Limits.MaxProcs = 48
	sb := h.create(t, spec)

	res, err := sb.Exec(h.ctx, contracts.ExecRequest{
		Command: []string{"/bin/sh", "-c", "f(){ f|f& };f; sleep 30"},
		Timeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("fork bomb should run into the timeout, got %+v", res)
	}
	top, err := h.cli.ContainerTop(h.ctx, sb.ID(), client.ContainerTopOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(top.Processes); n > 2 {
		t.Errorf("%d processes remain after the kill", n)
	}
	if got := h.out(t, sb, "cat /sys/fs/cgroup/pids.max; cat /sys/fs/cgroup/pids.current"); !strings.HasPrefix(got, "48\n") {
		t.Errorf("pids cgroup: %q", got)
	}
	if got := h.out(t, sb, "echo alive"); got != "alive" {
		t.Errorf("sandbox unusable after fork bomb: %q", got)
	}
}

func TestAttackDiskQuotaHolds(t *testing.T) {
	h := newHarness(t)
	spec := h.spec(t, contracts.NetworkNone)
	spec.Limits.DiskBytes = 32 << 20
	sb := h.create(t, spec)

	for _, dir := range []string{"/workspace", "/home/agent", "/tmp", "/dev/shm"} {
		res := h.sh(t, sb, "dd if=/dev/zero of="+dir+"/fill bs=1M count=40 2>/dev/null; echo dd=$?; wc -c < "+dir+"/fill; rm -f "+dir+"/fill")
		lines := strings.Fields(string(res.Stdout))
		if len(lines) != 2 || lines[0] != "dd=1" {
			t.Errorf("%s: filling should fail, got %q", dir, res.Stdout)
			continue
		}
		size, _ := strconv.Atoi(lines[1])
		if size > int(spec.Limits.DiskBytes) {
			t.Errorf("%s: %d bytes written, more than the whole quota", dir, size)
		}
	}
	// Every writable location, filled together, must stay within the quota
	// plus the fixed /dev/shm allowance.
	res := h.sh(t, sb, `for d in /workspace /home/agent /tmp; do dd if=/dev/zero of=$d/fill bs=1M count=40 2>/dev/null; done; cat /workspace/fill /home/agent/fill /tmp/fill | wc -c`)
	total, _ := strconv.Atoi(strings.TrimSpace(string(res.Stdout)))
	if total > int(spec.Limits.DiskBytes) {
		t.Errorf("%d bytes written across mounts, quota is %d", total, spec.Limits.DiskBytes)
	}
	if got := h.out(t, sb, "rm /workspace/fill /home/agent/fill /tmp/fill; echo cleaned"); got != "cleaned" {
		t.Error(got)
	}
	res = h.sh(t, sb, "touch /usr/fill /etc/fill /var/fill 2>&1; echo exit=$?")
	if !strings.Contains(string(res.Stdout), "exit=1") {
		t.Errorf("root filesystem writable: %s", res.Stdout)
	}
}

func TestAttackWorkerEnvironmentNotInherited(t *testing.T) {
	t.Setenv("CONCLAVE_WORKER_CANARY", "canary_worker_environment")
	t.Setenv("GITHUB_TOKEN", "canary_github_token")
	h := newHarness(t)
	sb := h.create(t, h.spec(t, contracts.NetworkNone))

	env := h.out(t, sb, "env | sort")
	for _, leak := range []string{"CONCLAVE_WORKER_CANARY", "GITHUB_TOKEN", "canary_worker", "DOCKER_HOST"} {
		if strings.Contains(env, leak) {
			t.Errorf("worker environment leaked %s:\n%s", leak, env)
		}
	}
	for _, want := range []string{"CI=true", "HOME=/home/agent", "TALLY_API_KEY=canary_tally_api_key", "GOFLAGS=-mod=mod"} {
		if !strings.Contains(env, want) {
			t.Errorf("allowlisted %s missing:\n%s", want, env)
		}
	}
}

func TestAttackFileOperationsRefuseSymlinks(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, h.spec(t, contracts.NetworkNone))

	h.out(t, sb, "ln -s /etc/passwd leak; mkdir -p real; ln -s /tmp escape; ln -s ../../../tmp relative; echo secret > /tmp/target")
	for _, path := range []string{"leak", "escape/target", "relative/target"} {
		if data, err := sb.ReadFile(h.ctx, path); !errors.Is(err, docker.ErrInvalidPath) {
			t.Errorf("ReadFile(%q) = %q, %v; want ErrInvalidPath", path, data, err)
		}
		if err := sb.WriteFile(h.ctx, path, []byte("x")); !errors.Is(err, docker.ErrInvalidPath) {
			t.Errorf("WriteFile(%q) = %v; want ErrInvalidPath", path, err)
		}
	}
	if got := h.out(t, sb, "cat /tmp/target; ls /tmp"); got != "secret\ntarget" {
		t.Errorf("a write escaped the workspace: %q", got)
	}
	if err := sb.WriteFile(h.ctx, "real/ok.txt", []byte("fine")); err != nil {
		t.Errorf("ordinary write failed: %v", err)
	}
}

// TestAttackFilePathsAreNeverExpanded proves the path reaches the script
// as data: a glob that would match .git does not, and a file whose name
// holds glob characters is addressed literally.
func TestAttackFilePathsAreNeverExpanded(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, h.spec(t, contracts.NetworkNone))

	// No file matches these names literally, so a read must fail; a write
	// creates the literal name and must leave .git alone.
	for _, path := range []string{".gi*/config", ".gi?/config", "[.]git/config", ".git*", "*/config", "src/*"} {
		if data, err := sb.ReadFile(h.ctx, path); err == nil {
			t.Errorf("ReadFile(%q) expanded the pattern and returned %q", path, data)
		}
		if err := sb.WriteFile(h.ctx, path, []byte("x")); err != nil {
			t.Errorf("WriteFile(%q) should create the literal name: %v", path, err)
		}
	}
	if got := h.out(t, sb, "grep -c '^x$' .git/config || true; ls .git/config; cat '.gi*/config' '[.]git/config' '.git*' 'src/*'"); got != "0\n.git/config\nxxxx" {
		t.Errorf(".git was touched or literal names were not written: %q", got)
	}

	literal := []string{"lit/a*b.txt", "lit/q?.txt", "lit/[set].txt", "lit/dir*/in?[x].txt"}
	for _, path := range literal {
		if err := sb.WriteFile(h.ctx, path, []byte("literal "+path)); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
		if got, err := sb.ReadFile(h.ctx, path); err != nil || string(got) != "literal "+path {
			t.Errorf("ReadFile(%q) = %q, %v", path, got, err)
		}
	}
	if got := h.out(t, sb, `ls -1 lit | grep -c '[*?\[]'; ls -1 'lit/dir*'`); got != "4\nin?[x].txt" {
		t.Errorf("literal names were not created as given:\n%s", got)
	}
}

// TestAttackSymlinkSwapRaceStaysInsideTheContainer attempts the
// check-then-use race the contract mentions: a background loop swaps a
// directory for a symlink to /tmp while WriteFile targets a file under it.
// The provider cannot close that race, so the test asserts the damage is
// bounded instead: every write lands inside the container, the worker's
// tree is untouched, and the sandbox keeps working.
func TestAttackSymlinkSwapRaceStaysInsideTheContainer(t *testing.T) {
	h := newHarness(t)
	spec := h.spec(t, contracts.NetworkNone)
	sb := h.create(t, spec)

	h.out(t, sb, `mkdir -p swap; (while :; do rm -rf swap; ln -s /tmp swap; rm -f swap; mkdir swap; done) >/dev/null 2>&1 & echo started`)
	refused := 0
	for i := 0; i < 60; i++ {
		if err := sb.WriteFile(h.ctx, "swap/target.txt", []byte("racing")); errors.Is(err, docker.ErrInvalidPath) {
			refused++
		}
		if data, err := os.ReadFile("/tmp/target.txt"); err == nil && string(data) == "racing" {
			t.Fatal("a sandbox write reached the worker's /tmp")
		}
	}
	res := h.sh(t, sb, "kill -9 -1 2>/dev/null; [ -f /tmp/target.txt ] && echo escaped-inside; echo done")
	t.Logf("%d writes refused by the check; escaped into the container's /tmp: %v", refused, strings.Contains(string(res.Stdout), "escaped-inside"))

	if data, err := os.ReadFile(filepath.Join(spec.TreePath, "README.md")); err != nil || string(data) != "# Election Tally\n" {
		t.Errorf("worker tree changed: %q, %v", data, err)
	}
	if got := h.out(t, sb, "echo alive"); got != "alive" {
		t.Errorf("sandbox unusable after the race: %q", got)
	}
}

func TestAttackUnpreparedTreeIsRefused(t *testing.T) {
	h := newHarness(t)
	spec := h.spec(t, contracts.NetworkNone)
	spec.TreePath, _ = sandboxtest.NewRepo(t)

	if _, err := h.p.Create(h.ctx, spec); err == nil {
		t.Fatal("Create accepted a tree with a remote and credentials")
	}
	if n := len(h.containers(t)); n != 0 {
		t.Errorf("%d containers created for a refused tree", n)
	}
}

func TestAttackNoTokenReachesTheSandbox(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, h.spec(t, contracts.NetworkNone))
	res := h.sh(t, sb, "grep -r "+sandboxtest.CanaryToken+" /workspace /home/agent /tmp /etc 2>/dev/null; env | grep -c "+sandboxtest.CanaryToken+" || true")
	if strings.TrimSpace(string(res.Stdout)) != "0" {
		t.Errorf("installation token found inside the sandbox:\n%s", res.Stdout)
	}
	if got := h.out(t, sb, "ls .git/modules/lib/config lib/lib.go"); got != ".git/modules/lib/config\nlib/lib.go" {
		t.Errorf("submodule did not travel with the tree: %q", got)
	}
}
