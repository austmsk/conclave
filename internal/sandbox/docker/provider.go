package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/sandbox"
)

// ErrInstallFailed reports an install command that exited non-zero. The
// sandbox has been closed; the results carry its output.
var ErrInstallFailed = errors.New("install command failed")

// Config tunes a Provider. The zero value is usable.
type Config struct {
	// Namespace scopes Reap's first sweep: it removes sandboxes created
	// with the same namespace at the requested age. Defaults to "default".
	Namespace string

	// StaleAge is the age past which Reap removes any Conclave sandbox
	// regardless of namespace, so a namespace change cannot strand
	// containers. Defaults to 24 hours.
	StaleAge time.Duration

	// UID and GID are the identity every process in the sandbox runs as.
	// Both default to 1000 and neither may be 0.
	UID, GID int

	// MaxOutputBytes caps each of stdout and stderr per exec, and the size
	// of a file ReadFile will return. A command that exceeds it is killed
	// and Exec returns ErrOutputTooLarge. Defaults to 32 MiB.
	MaxOutputBytes int64

	// Client, when set, is used instead of one built from the environment.
	Client *client.Client
}

func (c Config) withDefaults() Config {
	if c.Namespace == "" {
		c.Namespace = "default"
	}
	if c.StaleAge <= 0 {
		c.StaleAge = 24 * time.Hour
	}
	if c.UID == 0 {
		c.UID = 1000
	}
	if c.GID == 0 {
		c.GID = 1000
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = 32 << 20
	}
	return c
}

// Provider creates Docker-backed sandboxes.
type Provider struct {
	cli       *client.Client
	cfg       Config
	ownClient bool
}

var _ contracts.SandboxProvider = (*Provider)(nil)

// New connects to the Docker daemon and verifies it answers. The daemon
// location comes from the worker's DOCKER_* environment, which is worker
// configuration and never reaches a sandbox.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	cfg = cfg.withDefaults()
	p := &Provider{cli: cfg.Client, cfg: cfg}
	if p.cli == nil {
		cli, err := client.New(client.FromEnv)
		if err != nil {
			return nil, fmt.Errorf("creating docker client: %w", err)
		}
		p.cli, p.ownClient = cli, true
	}
	if _, err := p.cli.Ping(ctx, client.PingOptions{}); err != nil {
		return nil, fmt.Errorf("pinging docker daemon: %w", err)
	}
	return p, nil
}

// Close releases the client if the provider created it.
func (p *Provider) Close() error {
	if !p.ownClient {
		return nil
	}
	return p.cli.Close()
}

// Create builds a hardened container with no network, streams the
// prepared working tree into its workspace, and returns the running
// sandbox. A spec asking for NetworkRegistries is refused here: nothing
// on contracts.Sandbox can cut a network, so a networked sandbox must not
// leave this package. Use CreateWithInstall, which runs the install phase
// and disconnects before returning.
func (p *Provider) Create(ctx context.Context, spec contracts.SandboxSpec) (contracts.Sandbox, error) {
	if spec.Network == contracts.NetworkRegistries {
		return nil, fmt.Errorf("%w: NetworkRegistries requires CreateWithInstall, so the network is cut before the sandbox is returned", ErrInvalidSpec)
	}
	return p.create(ctx, spec)
}

// CreateWithInstall creates the sandbox, runs the install commands in
// order under the spec's network mode, cuts the network, and returns the
// sandbox ready for the agent phase. The results of every command run are
// returned in both outcomes. A command that exits non-zero, or a failure
// at any step, closes the sandbox and returns ErrInstallFailed or the
// step's error; the caller never receives a sandbox with a network.
func (p *Provider) CreateWithInstall(ctx context.Context, spec contracts.SandboxSpec, install []contracts.ExecRequest) (contracts.Sandbox, []contracts.ExecResult, error) {
	sb, err := p.create(ctx, spec)
	if err != nil {
		return nil, nil, err
	}
	results, err := p.installPhase(ctx, sb, install)
	if err != nil {
		_ = sb.Close(context.WithoutCancel(ctx))
		return nil, results, err
	}
	return sb, results, nil
}

func (p *Provider) installPhase(ctx context.Context, sb *Sandbox, install []contracts.ExecRequest) ([]contracts.ExecResult, error) {
	results := make([]contracts.ExecResult, 0, len(install))
	for i, req := range install {
		res, err := sb.Exec(ctx, req)
		results = append(results, res)
		if err != nil {
			return results, fmt.Errorf("install step %d: %w", i+1, err)
		}
		if res.ExitCode != 0 {
			return results, fmt.Errorf("%w: step %d exited %d", ErrInstallFailed, i+1, res.ExitCode)
		}
	}
	if err := sb.disconnectNetwork(ctx); err != nil {
		return results, err
	}
	return results, nil
}

func (p *Provider) create(ctx context.Context, spec contracts.SandboxSpec) (*Sandbox, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	base, err := sandbox.HeadCommit(spec.TreePath)
	if err != nil {
		return nil, fmt.Errorf("resolving base commit: %w", err)
	}
	volumes, err := p.ensureImage(ctx, spec.Image)
	if err != nil {
		return nil, err
	}
	name := "conclave-" + p.cfg.Namespace + "-" + randomSuffix()
	sb := &Sandbox{
		cli:       p.cli,
		name:      name,
		base:      base,
		limits:    spec.Limits,
		deadline:  time.Now().Add(spec.Limits.Timeout),
		maxOutput: p.cfg.MaxOutputBytes,
		uid:       p.cfg.UID,
		gid:       p.cfg.GID,
	}
	if err := p.start(ctx, sb, spec, volumes); err != nil {
		// Cleanup uses a context that survives the caller's cancellation,
		// or a cancelled Create would be the first orphan.
		_ = sb.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return sb, nil
}

func (p *Provider) start(ctx context.Context, sb *Sandbox, spec contracts.SandboxSpec, volumes []string) error {
	networkMode := "none"
	if spec.Network == contracts.NetworkRegistries {
		net, err := p.cli.NetworkCreate(ctx, sb.name, client.NetworkCreateOptions{
			Driver: "bridge",
			Labels: p.labels(),
		})
		if err != nil {
			return fmt.Errorf("creating network for sandbox %s: %w", sb.name, err)
		}
		sb.networkID = net.ID
		networkMode = net.ID
	}
	created, err := p.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       sb.name,
		Config:     p.containerConfig(spec),
		HostConfig: p.hostConfig(spec, networkMode, volumes),
	})
	if err != nil {
		return fmt.Errorf("creating container for sandbox %s: %w", sb.name, err)
	}
	sb.id = created.ID
	if err := p.verifyNoMounts(ctx, sb); err != nil {
		return err
	}
	if _, err := p.cli.ContainerStart(ctx, sb.id, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("starting container for sandbox %s: %w", sb.name, err)
	}
	if err := sb.copyTreeIn(ctx, spec.TreePath); err != nil {
		return fmt.Errorf("copying working tree into sandbox %s: %w", sb.name, err)
	}
	return nil
}

// ensureImage pulls the image if the daemon does not have it and returns
// the volume paths the image declares, sorted. The reference is
// digest-pinned, so what arrives is what was asked for.
func (p *Provider) ensureImage(ctx context.Context, image string) ([]string, error) {
	info, err := p.cli.ImageInspect(ctx, image)
	if cerrdefs.IsNotFound(err) {
		resp, perr := p.cli.ImagePull(ctx, image, client.ImagePullOptions{})
		if perr != nil {
			return nil, fmt.Errorf("pulling image: %w", perr)
		}
		if perr := resp.Wait(ctx); perr != nil {
			return nil, fmt.Errorf("pulling image: %w", perr)
		}
		info, err = p.cli.ImageInspect(ctx, image)
	}
	if err != nil {
		return nil, fmt.Errorf("inspecting image: %w", err)
	}
	var volumes []string
	if info.Config != nil {
		for path := range info.Config.Volumes {
			volumes = append(volumes, path)
		}
	}
	sort.Strings(volumes)
	return volumes, nil
}

// verifyNoMounts refuses a container the daemon gave any mount at all. The
// host configuration asks for none, and image-declared volumes are covered
// by read-only tmpfs, so anything listed here is storage outside the
// sandbox's quota and lifetime.
func (p *Provider) verifyNoMounts(ctx context.Context, sb *Sandbox) error {
	info, err := p.cli.ContainerInspect(ctx, sb.id, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspecting container for sandbox %s: %w", sb.name, err)
	}
	if len(info.Container.Mounts) > 0 {
		m := info.Container.Mounts[0]
		return fmt.Errorf("%w: image gives the container a %s mount at %s", ErrInvalidSpec, m.Type, m.Destination)
	}
	return nil
}

// Reap removes orphaned sandboxes and returns how many containers it
// removed (NFR-R3). It sweeps twice: containers and networks in the
// provider's namespace older than olderThan, then any Conclave sandbox in
// any namespace older than Config.StaleAge, so a namespace change cannot
// strand containers. It relies on labels rather than in-memory state, so
// it also finds sandboxes created by a worker that has since died. A
// removal that fails is reported and does not stop the sweep, so one
// wedged container cannot shield every orphan behind it.
func (p *Provider) Reap(ctx context.Context, olderThan time.Duration) (int, error) {
	now := time.Now()
	sweeps := []struct {
		filter string
		cutoff time.Time
	}{
		{labelNamespace + "=" + p.cfg.Namespace, now.Add(-olderThan)},
		{labelSandbox + "=true", now.Add(-max(olderThan, p.cfg.StaleAge))},
	}
	removed := 0
	var errs []error
	for _, sweep := range sweeps {
		filters := make(client.Filters).Add("label", sweep.filter)
		n, err := p.reapContainers(ctx, filters, sweep.cutoff)
		removed += n
		errs = append(errs, err, p.reapNetworks(ctx, filters, sweep.cutoff))
	}
	return removed, errors.Join(errs...)
}

func (p *Provider) reapContainers(ctx context.Context, filters client.Filters, cutoff time.Time) (int, error) {
	containers, err := p.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return 0, fmt.Errorf("listing sandbox containers: %w", err)
	}
	removed := 0
	var errs []error
	for _, c := range containers.Items {
		if time.Unix(c.Created, 0).After(cutoff) {
			continue
		}
		_, err := p.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		switch {
		case err == nil:
			removed++
		case cerrdefs.IsNotFound(err):
			// Already gone: not ours to count.
		default:
			errs = append(errs, fmt.Errorf("removing orphaned container %s: %w", c.ID, err))
		}
	}
	return removed, errors.Join(errs...)
}

func (p *Provider) reapNetworks(ctx context.Context, filters client.Filters, cutoff time.Time) error {
	networks, err := p.cli.NetworkList(ctx, client.NetworkListOptions{Filters: filters})
	if err != nil {
		return fmt.Errorf("listing sandbox networks: %w", err)
	}
	var errs []error
	for _, n := range networks.Items {
		if n.Created.After(cutoff) {
			continue
		}
		if _, err := p.cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("removing orphaned network %s: %w", n.ID, err))
		}
	}
	return errors.Join(errs...)
}

func randomSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not recoverable; a fixed suffix would
		// collide on the next call, which Docker reports as a conflict.
		panic(errors.Join(errors.New("reading random bytes"), err))
	}
	return hex.EncodeToString(b[:])
}
