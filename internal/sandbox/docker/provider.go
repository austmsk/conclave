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

// Config tunes a Provider. The zero value is usable.
type Config struct {
	// Namespace scopes Reap: it only removes sandboxes created with the
	// same namespace. Defaults to "default".
	Namespace string

	// UID and GID are the identity every process in the sandbox runs as.
	// Both default to 1000 and neither may be 0.
	UID, GID int

	// MaxOutputBytes caps each of stdout and stderr per exec. A command
	// that exceeds it is killed and Exec returns ErrOutputTooLarge.
	// Defaults to 32 MiB.
	MaxOutputBytes int64

	// Client, when set, is used instead of one built from the environment.
	Client *client.Client
}

func (c Config) withDefaults() Config {
	if c.Namespace == "" {
		c.Namespace = "default"
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

// Create builds a hardened container, streams the prepared working tree
// into its workspace, and returns the running sandbox. Any failure after a
// resource was allocated releases it before returning.
func (p *Provider) Create(ctx context.Context, spec contracts.SandboxSpec) (contracts.Sandbox, error) {
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
	for _, m := range info.Container.Mounts {
		return fmt.Errorf("%w: image gives the container a %s mount at %s", ErrInvalidSpec, m.Type, m.Destination)
	}
	return nil
}

// Reap removes every container and network in the provider's namespace
// created more than olderThan ago, whatever state it is in, and returns
// the number of containers removed (NFR-R3). It relies on labels rather
// than in-memory state, so it also finds sandboxes created by a worker
// that has since died.
func (p *Provider) Reap(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan)
	filters := make(client.Filters).Add("label", labelNamespace+"="+p.cfg.Namespace)

	containers, err := p.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return 0, fmt.Errorf("listing sandbox containers: %w", err)
	}
	removed := 0
	for _, c := range containers.Items {
		if time.Unix(c.Created, 0).After(cutoff) {
			continue
		}
		_, err := p.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		if err != nil && !cerrdefs.IsNotFound(err) {
			return removed, fmt.Errorf("removing orphaned container %s: %w", c.ID, err)
		}
		removed++
	}

	networks, err := p.cli.NetworkList(ctx, client.NetworkListOptions{Filters: filters})
	if err != nil {
		return removed, fmt.Errorf("listing sandbox networks: %w", err)
	}
	for _, n := range networks.Items {
		if n.Created.After(cutoff) {
			continue
		}
		if _, err := p.cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			return removed, fmt.Errorf("removing orphaned network %s: %w", n.ID, err)
		}
	}
	return removed, nil
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
