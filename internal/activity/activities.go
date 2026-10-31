package activity

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/austmsk/conclave/internal/agent"
	"github.com/austmsk/conclave/internal/blob"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/gate"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/sandbox"
	"github.com/austmsk/conclave/internal/store"
)

// The parts of RunAttempt's StartToCloseTimeout besides the agent's wall
// clock and the gate commands (decision 0012). With the shipped role and
// a five-minute command timeout the sum is 45 minutes, above the router's
// ten-minute call bound (llm.MaxCallDuration).
const (
	// SandboxCreateBudget covers the clone, the sandbox create and its
	// install phase.
	SandboxCreateBudget = 3 * time.Minute
	// StagingBudget covers freezing the artifact and staging it for the
	// command gates.
	StagingBudget = 2 * time.Minute
	// AttemptMargin covers recording the outcome and releasing the
	// sandbox.
	AttemptMargin = 5 * time.Minute

	// HeartbeatEvery is how often RunAttempt heartbeats, from its own
	// goroutine, whatever the agent is doing, including waiting on the
	// router's rate limiter, which has no heartbeat hook of its own.
	HeartbeatEvery = 30 * time.Second
	// HeartbeatTimeout is how long Temporal waits between heartbeats
	// before it treats the worker running an attempt as dead.
	HeartbeatTimeout = 2 * time.Minute
)

// AttemptTimeout is RunAttempt's StartToCloseTimeout for a role and a
// repository's gate command timeout: sandbox create and install, the
// agent's wall clock, three gate commands, staging, and a margin.
func AttemptTimeout(spec contracts.RoleSpec, commandTimeout time.Duration) time.Duration {
	return SandboxCreateBudget + spec.Limits.MaxWallClock + 3*commandTimeout + StagingBudget + AttemptMargin
}

// Repo is one repository's configuration, from the four deploy files
// that name it. The worker builds it at startup and refuses to start if
// the files do not name the same repositories.
type Repo struct {
	Access    llm.RepoAccess
	Sandbox   sandbox.RepoSandbox
	Toolchain agent.Toolchain
	Gates     gate.Policy
}

// GateFunc gates one frozen artifact: (*gate.Gater).Gate in production.
type GateFunc func(ctx context.Context, in gate.Input) (gate.Outcome, error)

// Deps are the activities' clients and configuration.
type Deps struct {
	// DB connects as the application role.
	DB        store.QueryTxDB
	GitHub    github.Client
	Sandboxes contracts.SandboxProvider
	Router    *llm.Router
	// Trees prepares the tree an attempt runs on. The attempt names its
	// repository by full name only, so a task needs no GitHub state of
	// its own; GitHubTrees clones through the App.
	Trees TreeSource
	// Roles maps each role a task may run to its spec. The worker fills
	// it; an attempt for a role not in it is refused.
	Roles map[contracts.Role]contracts.RoleSpec
	// RouterConfig is the router's validated configuration; attempts
	// record the model and provider it routes their role to.
	RouterConfig llm.Config
	Blobs        blob.Store
	Gate         GateFunc

	// Repos is keyed by full name.
	Repos map[string]Repo
	// OwnerUserID is the owner's numeric GitHub user ID. Only an issue
	// the owner wrote is planned from its sections.
	OwnerUserID int64
	// ConfigDigests maps each deploy file's name to the SHA-256 of the
	// bytes the worker validated. Every plan body records them.
	ConfigDigests map[string]string
	// WorkDir is where trees are cloned. Each clone gets its own
	// directory under it and is removed when its activity ends.
	WorkDir string

	// HeartbeatEvery overrides the heartbeat interval; zero means
	// HeartbeatEvery. Tests shorten it.
	HeartbeatEvery time.Duration
}

// Activities holds every activity the workflows call. Register it with a
// worker; Temporal names each activity after its method.
type Activities struct {
	d           Deps
	toolCalls   *store.ToolCalls
	gateResults *store.GateResults
	repos       *store.RepoDirectory
	ledger      *store.CostLedger
}

// New validates d and returns the activities.
func New(d Deps) (*Activities, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	if d.HeartbeatEvery <= 0 {
		d.HeartbeatEvery = HeartbeatEvery
	}
	d.Repos = maps.Clone(d.Repos)
	d.Roles = maps.Clone(d.Roles)
	d.ConfigDigests = maps.Clone(d.ConfigDigests)
	return &Activities{
		d:           d,
		toolCalls:   store.NewToolCalls(d.DB),
		gateResults: store.NewGateResults(d.DB),
		repos:       store.NewRepoDirectory(d.DB),
		ledger:      store.NewCostLedger(d.DB),
	}, nil
}

func (d Deps) validate() error {
	switch {
	case d.DB == nil || d.GitHub == nil || d.Trees == nil || d.Sandboxes == nil || d.Router == nil || d.Blobs == nil || d.Gate == nil:
		return errors.New("activities need a database, GitHub, tree source, sandbox provider, router, blob store and gate")
	case len(d.Repos) == 0:
		return errors.New("activities need at least one configured repository")
	case len(d.Roles) == 0:
		return errors.New("activities need at least one role spec")
	case d.OwnerUserID <= 0:
		return errors.New("activities need the owner's GitHub user ID")
	case d.WorkDir == "":
		return errors.New("activities need a work directory for clones")
	}
	for _, role := range slices.Sorted(maps.Keys(d.Roles)) {
		if spec := d.Roles[role]; spec.Role != role {
			return fmt.Errorf("activities: role %s maps to the spec for %s", role, spec.Role)
		}
		if _, err := d.provenance(role); err != nil {
			return fmt.Errorf("activities: %w", err)
		}
	}
	return nil
}

// repo returns a configured repository's settings.
func (a *Activities) repo(fullName string) (Repo, error) {
	r, ok := a.d.Repos[fullName]
	if !ok {
		return Repo{}, fmt.Errorf("%w: repository %s is not configured on this worker", errNonRetryable, fullName)
	}
	return r, nil
}

// roleSpec returns the spec the worker configured for role. Milestone 1's
// worker configures the implementer; Milestone 2 adds the test writer to
// the worker's map, with no change here.
func (d Deps) roleSpec(role contracts.Role) (contracts.RoleSpec, error) {
	spec, ok := d.Roles[role]
	if !ok {
		return contracts.RoleSpec{}, fmt.Errorf("%w: no role spec for role %q on this worker", errNonRetryable, role)
	}
	return spec, nil
}

// LongestAttempt is the longest StartToCloseTimeout any attempt on this
// worker can be given. The startup sweep's cutoff must exceed it, or the
// sweep would release a live attempt's sandbox.
// It is a method on Deps, not Activities, because the worker registers
// every exported method of Activities as an activity.
func (d Deps) LongestAttempt() time.Duration {
	var longest time.Duration
	for _, spec := range d.Roles {
		for _, r := range d.Repos {
			longest = max(longest, AttemptTimeout(spec, r.Sandbox.CommandTimeout))
		}
	}
	return longest
}

// provenance is what the router is configured to use for role: the
// prompt version from its spec, and its primary model and that model's
// provider. FinishAttempt later records the model that actually answered.
func (d Deps) provenance(role contracts.Role) (contracts.Provenance, error) {
	spec, err := d.roleSpec(role)
	if err != nil {
		return contracts.Provenance{}, err
	}
	route, ok := d.RouterConfig.Roles[role]
	if !ok {
		return contracts.Provenance{}, fmt.Errorf("%w: router has no route for role %s", errNonRetryable, role)
	}
	model, ok := d.RouterConfig.Models[route.Model]
	if !ok {
		return contracts.Provenance{}, fmt.Errorf("%w: router route for %s names unknown model %s", errNonRetryable, role, route.Model)
	}
	return contracts.Provenance{Role: role, PromptVersion: spec.PromptVersion, Model: route.Model, Provider: model.Provider}, nil
}
