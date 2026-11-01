package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	sdkactivity "go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/agent"
	"github.com/austmsk/conclave/internal/alert"
	"github.com/austmsk/conclave/internal/blob"
	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/gate"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/llm/anthropic"
	"github.com/austmsk/conclave/internal/sandbox/docker"
	"github.com/austmsk/conclave/internal/store"
	"github.com/austmsk/conclave/internal/workflow"
)

// Secrets the worker reads from its environment, set by the owner's
// secrets file outside the repository. Each is read once and unset, so
// nothing the worker starts later can inherit it.
const (
	databaseURLEnv  = "CONCLAVE_DATABASE_URL"
	anthropicKeyEnv = "CONCLAVE_ANTHROPIC_API_KEY"
)

// sweepAfter is the default age past which the startup sweep releases an
// unreleased sandbox. It must exceed the longest attempt (45 minutes with
// the shipped configuration), or the sweep would remove a live attempt's
// sandbox from under it on a second worker.
const sweepAfter = 2 * time.Hour

// options are the worker's flags.
type options struct {
	deploy         paths
	gitleaksConfig string
	blobDir        string
	workDir        string
	temporalAddr   string
	namespace      string
	sweepAfter     time.Duration
	reapEvery      time.Duration
	haltGrace      time.Duration
	alertLog       string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "conclave worker:", err)
		os.Exit(1)
	}
}

func parseFlags() options {
	var o options
	flag.StringVar(&o.deploy.Router, "router", "deploy/router.yaml", "router configuration")
	flag.StringVar(&o.deploy.Repos, "repos", "deploy/repos.yaml", "repository access file")
	flag.StringVar(&o.deploy.Toolchains, "toolchains", "deploy/toolchains.yaml", "toolchains and repository profiles")
	flag.StringVar(&o.deploy.Sandboxes, "sandboxes", "deploy/sandboxes.yaml", "sandbox configuration")
	flag.StringVar(&o.deploy.Gates, "gates", "deploy/gates.yaml", "gate policy")
	flag.StringVar(&o.deploy.GitHub, "github-config", "deploy/github.yaml", "GitHub App configuration")
	flag.StringVar(&o.gitleaksConfig, "gitleaks-config", "deploy/gitleaks.toml", "gitleaks rules")
	flag.StringVar(&o.blobDir, "blob-dir", "", "blob store directory (required)")
	flag.StringVar(&o.workDir, "work-dir", "", "scratch directory for clones (required)")
	flag.StringVar(&o.temporalAddr, "temporal-address", client.DefaultHostPort, "Temporal frontend address")
	flag.StringVar(&o.namespace, "temporal-namespace", client.DefaultNamespace, "Temporal namespace")
	flag.DurationVar(&o.sweepAfter, "sweep-after", sweepAfter, "age past which the sweep releases an unreleased sandbox")
	flag.DurationVar(&o.reapEvery, "reap-every", reapEvery, "interval of the scheduled reaper")
	flag.DurationVar(&o.haltGrace, "halt-grace", haltGrace, "how long the worker keeps running after it sees a halt")
	flag.StringVar(&o.alertLog, "alert-log", "", "local append-only alert log (required)")
	flag.Parse()
	return o
}

func run() error {
	o := parseFlags()
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: github.RedactAttr}))
	if o.blobDir == "" || o.workDir == "" || o.alertLog == "" {
		return errors.New("-blob-dir, -work-dir and -alert-log are required")
	}
	if err := checkHaltGrace(o.haltGrace); err != nil {
		return err
	}
	d, err := loadDeploy(o.deploy)
	if err != nil {
		return err
	}
	if err := checkAlerts(d); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sec, err := readSecrets()
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, sec.databaseURL)
	if err != nil {
		return fmt.Errorf("connecting to Postgres: %w", err)
	}
	defer pool.Close()
	halt := func(ctx context.Context) (store.HaltState, error) { return store.CurrentHalt(ctx, pool) }
	if err := refuseIfHalted(ctx, halt); err != nil {
		return err
	}
	if err := seedRepos(ctx, pool, d); err != nil {
		return err
	}
	alerts, err := alert.OpenLog(o.alertLog)
	if err != nil {
		return err
	}
	defer func() { _ = alerts.Close() }()
	acts, sandboxes, err := newActivities(ctx, o, d, sec, pool, alerts)
	if err != nil {
		return err
	}
	res, err := activity.SweepOrphans(ctx, pool, sandboxes, o.sweepAfter, log)
	log.Info("startup sweep", "released", res.Released, "reaped", res.Reaped, "error", errString(err))
	return serve(ctx, o, acts, halt, log)
}

// checkAlerts requires the alerts repository, and refuses one that is also
// a run repository: its token asks only for issues, and no agent may ever
// work where alerts are read (RL-18).
func checkAlerts(d deploy) error {
	if err := d.GitHub.RequireAlerts(); err != nil {
		return err
	}
	if _, runs := d.Access[d.GitHub.Alerts.Repository]; runs {
		return fmt.Errorf("alerts.repository %s is also a run repository in repos.yaml; alerts need a repository of their own", d.GitHub.Alerts.Repository)
	}
	return nil
}

// secrets are the values the worker takes from its environment.
type secrets struct {
	databaseURL, anthropicKey, appKeyFile string
}

// readSecrets reads and unsets each secret variable. A missing one stops
// startup by name, never by value.
func readSecrets() (secrets, error) {
	var s secrets
	vars := []struct {
		name string
		dst  *string
	}{
		{databaseURLEnv, &s.databaseURL}, {anthropicKeyEnv, &s.anthropicKey}, {github.AppKeyFileEnv, &s.appKeyFile},
	}
	for _, v := range vars {
		*v.dst = os.Getenv(v.name)
		if err := os.Unsetenv(v.name); err != nil {
			return secrets{}, fmt.Errorf("unsetting %s: %w", v.name, err)
		}
		if *v.dst == "" {
			return secrets{}, fmt.Errorf("%s is not set", v.name)
		}
	}
	return s, nil
}

// seedRepos makes the repos table match configuration, which is the
// authority for each repository's tier and default branch (FR-SEC12).
func seedRepos(ctx context.Context, db store.DBTX, d deploy) error {
	for _, name := range slices.Sorted(maps.Keys(d.Access)) {
		_, err := store.EnsureRepo(ctx, db, contracts.Repo{
			FullName: name, Tier: d.Access[name].Tier, DefaultBranch: d.Sandboxes[name].DefaultBranch,
		})
		if err != nil {
			return fmt.Errorf("seeding repository %s: %w", name, err)
		}
	}
	return nil
}

// newActivities builds every client the activities use and validates the
// whole set.
func newActivities(ctx context.Context, o options, d deploy, sec secrets, pool *pgxpool.Pool, alerts *alert.Log) (*activity.Activities, *docker.Provider, error) {
	model, err := anthropic.New(sec.anthropicKey, "", nil)
	if err != nil {
		return nil, nil, fmt.Errorf("building the Anthropic client: %w", err)
	}
	router, err := llm.New(d.Router, map[string]llm.Provider{llm.ProviderAnthropic: model},
		llm.NewDirectoryRepos(store.NewRepoDirectory(pool), d.Access), store.NewCostLedger(pool))
	if err != nil {
		return nil, nil, fmt.Errorf("building the router: %w", err)
	}
	key, err := github.LoadAppKey(sec.appKeyFile)
	if err != nil {
		return nil, nil, err
	}
	gh, err := github.NewApp(d.GitHub, key, nil)
	if err != nil {
		return nil, nil, err
	}
	sandboxes, err := docker.New(ctx, docker.Config{})
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to Docker: %w", err)
	}
	blobs, err := blob.NewDir(o.blobDir)
	if err != nil {
		return nil, nil, err
	}
	scanner, err := gate.NewGitleaks(d.Gates.Gitleaks, o.gitleaksConfig)
	if err != nil {
		return nil, nil, err
	}
	gater := &gate.Gater{Blobs: blobs, Results: store.NewGateResults(pool), Rejections: store.NewToolCalls(pool), Secrets: scanner}
	deps := activity.Deps{
		DB: pool, GitHub: gh, Trees: activity.GitHubTrees{Client: gh, InstallationID: d.GitHub.InstallationID},
		Sandboxes: sandboxes, Router: router, RouterConfig: d.Router, Blobs: blobs, Gate: gater.Gate,
		Roles: roles(), Repos: d.repos(), OwnerUserID: d.GitHub.OwnerUserID, ConfigDigests: d.Digests, WorkDir: o.workDir,
		AlertLog: alerts, AlertIssues: gh,
	}
	deps.Bot.Name, deps.Bot.Email = d.GitHub.BotIdentity()
	if err := checkSweepAfter(o.sweepAfter, deps.LongestSandboxHold()); err != nil {
		return nil, nil, err
	}
	acts, err := activity.New(deps)
	return acts, sandboxes, err
}

// checkSweepAfter refuses a sweep cutoff that does not exceed the longest
// any activity holds a sandbox, an attempt or Finalize: the sweep would
// remove a live sandbox from under it on a second worker. The startup
// sweep and the scheduled reaper share the cutoff.
func checkSweepAfter(sweepAfter, longestHold time.Duration) error {
	if sweepAfter <= longestHold {
		return fmt.Errorf("-sweep-after %s must exceed the longest sandbox hold, %s, or the sweep would release a live sandbox", sweepAfter, longestHold)
	}
	return nil
}

// roles are the role specs this worker runs. Milestone 1 runs the
// implementer and the pr_writer; Milestone 2 adds the test writer here.
// Every role needs a route in the router configuration, or startup fails.
func roles() map[contracts.Role]contracts.RoleSpec {
	return map[contracts.Role]contracts.RoleSpec{
		contracts.RoleImplementer: agent.Implementer,
		contracts.RolePRWriter:    agent.PRWriter,
	}
}

// serve runs two workers until SIGINT, SIGTERM or a halt: one for the
// workflows and the small activities, and one for the attempt queue with
// a single slot, so attempts run one at a time and two issues labelled at
// once queue. It keeps the reaper's schedule in place first.
func serve(ctx context.Context, o options, acts *activity.Activities, halt haltState, log *slog.Logger) error {
	c, err := client.Dial(client.Options{HostPort: o.temporalAddr, Namespace: o.namespace, Logger: log})
	if err != nil {
		return fmt.Errorf("connecting to Temporal at %s: %w", o.temporalAddr, err)
	}
	defer c.Close()
	if err := ensureReaperSchedule(ctx, c.ScheduleClient(), o.reapEvery, o.sweepAfter); err != nil {
		return err
	}
	ctx, stopForHalt := context.WithCancelCause(ctx)
	go watchHalt(ctx, halt, haltPoll, o.haltGrace, func() { stopForHalt(errHalted) }, log)
	feature := worker.New(c, workflow.TaskQueue, worker.Options{})
	feature.RegisterWorkflow(workflow.FeatureWorkflow)
	feature.RegisterWorkflow(workflow.TaskWorkflow)
	feature.RegisterWorkflow(workflow.ClosedCommandWorkflow)
	feature.RegisterWorkflow(workflow.ReaperWorkflow)
	feature.RegisterActivity(acts)
	attempts := worker.New(c, workflow.AttemptTaskQueue, worker.Options{MaxConcurrentActivityExecutionSize: 1})
	attempts.RegisterActivityWithOptions(acts.RunAttempt, sdkactivity.RegisterOptions{Name: "RunAttempt"})
	// Finalize runs a sandbox and the gate commands, so it shares the
	// attempt queue's one slot.
	attempts.RegisterActivityWithOptions(acts.Finalize, sdkactivity.RegisterOptions{Name: "Finalize"})
	for _, w := range []worker.Worker{feature, attempts} {
		if err := w.Start(); err != nil {
			return fmt.Errorf("starting worker: %w", err)
		}
		defer w.Stop()
	}
	log.Info("conclave worker polling", "task_queues", []string{workflow.TaskQueue, workflow.AttemptTaskQueue})
	<-ctx.Done()
	if errors.Is(context.Cause(ctx), errHalted) {
		return errHalted
	}
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
