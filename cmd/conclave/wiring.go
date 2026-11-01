package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/alert"
	"github.com/austmsk/conclave/internal/deployfile"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/sandbox/docker"
	"github.com/austmsk/conclave/internal/store"
)

// alerter is the alert log and the App's alert calls.
type alerter struct {
	log    *alert.Log
	issues alert.Issues
}

func (a alerter) raise(ctx context.Context, al alert.Alert) error {
	_, err := alert.Raise(ctx, a.log, a.issues, al)
	return err
}

// openAlerter loads the GitHub configuration, which must name an alerts
// repository, the App key, and the alert log.
func openAlerter(o options, keyFile string) (alerter, error) {
	app, err := openAlertApp(o, keyFile)
	if err != nil {
		return alerter{}, err
	}
	log, err := alert.OpenLog(o.alertLog)
	if err != nil {
		return alerter{}, err
	}
	return alerter{log: log, issues: app}, nil
}

// openAlertApp is the App, from a GitHub configuration that names an
// alerts repository, and the App key.
func openAlertApp(o options, keyFile string) (*github.App, error) {
	if keyFile == "" {
		return nil, fmt.Errorf("%s is not set", github.AppKeyFileEnv)
	}
	var cfg github.Config
	if _, err := deployfile.Load(o.githubConfig, func(r io.Reader) (err error) { cfg, err = github.LoadConfig(r); return err }); err != nil {
		return nil, err
	}
	if err := cfg.RequireAlerts(); err != nil {
		return nil, err
	}
	key, err := github.LoadAppKey(keyFile)
	if err != nil {
		return nil, err
	}
	return github.NewApp(cfg, key, nil)
}

// unreachableIssues stands in for GitHub when the App cannot be built:
// alert.Raise still writes the alert to the local log, then records the
// delivery as failed with err.
type unreachableIssues struct{ err error }

func (u unreachableIssues) CreateAlertIssue(context.Context, string, string, []string) (github.Issue, error) {
	return github.Issue{}, u.err
}

func (u unreachableIssues) ListOpenAlertIssues(context.Context, string) ([]github.Issue, error) {
	return nil, u.err
}

func (u unreachableIssues) CommentOnAlertIssue(context.Context, int, string) (github.Comment, error) {
	return github.Comment{}, u.err
}

func (u unreachableIssues) AlertIssueComments(context.Context, int) ([]github.Comment, error) {
	return nil, u.err
}

func openPool(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connecting to Postgres: %w", err)
	}
	return pool, nil
}

// connectTimeout bounds each connection the halt makes after recording
// itself, so a hung Temporal or Docker cannot hold the kill switch.
const connectTimeout = 15 * time.Second

func dialTemporal(ctx context.Context, o options, timeout time.Duration, log *slog.Logger) (client.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, err := client.DialContext(ctx, client.Options{HostPort: o.temporalAddr, Namespace: o.namespace, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("connecting to Temporal at %s: %w", o.temporalAddr, err)
	}
	return c, nil
}

// newKillSwitch is the kill switch over Postgres, with every step that
// needs another system failing until connect, or wire, replaces it.
func newKillSwitch(o options, pool *pgxpool.Pool, out io.Writer) *killSwitch {
	k := &killSwitch{
		setHalt: func(ctx context.Context, r string) (store.HaltState, error) {
			return store.SetHalt(ctx, pool, r, store.ActorCLI)
		},
		clearHalt: func(ctx context.Context, r string) (store.HaltState, error) {
			return store.ClearHalt(ctx, pool, r, store.ActorCLI)
		},
		currentHalt: func(ctx context.Context) (store.HaltState, error) { return store.CurrentHalt(ctx, pool) },
		drain:       o.drain, poll: listPoll, now: time.Now, sleep: sleepCtx, out: out,
	}
	k.unavailable(errNotConnected)
	return k
}

// haltConnector builds, after the halt is recorded, each system the rest
// of the halt uses, one at a time and each bounded by connectTimeout. A
// system it cannot reach is reported and its steps keep failing; the halt
// goes on. Its closers release what it opened.
type haltConnector struct {
	o       options
	pool    *pgxpool.Pool
	keyFile string
	log     *slog.Logger
	// timeout bounds each connection; zero is connectTimeout.
	timeout time.Duration
	closers []func()
}

func (h *haltConnector) connect(ctx context.Context, k *killSwitch) {
	if h.timeout <= 0 {
		h.timeout = connectTimeout
	}
	if raise, err := h.alerts(); err != nil {
		k.raise = func(context.Context, alert.Alert) error { return err }
		k.printf("warning: alerts unavailable: %v", err)
	} else {
		k.raise = raise
	}
	if c, err := dialTemporal(ctx, h.o, h.timeout, h.log); err != nil {
		k.runs = unavailableRuns{err: err}
		k.printf("warning: %v; no workflow is cancelled by this halt", err)
	} else {
		h.closers = append(h.closers, c.Close)
		k.runs = temporalRuns{c: c, namespace: h.o.namespace}
	}
	dctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	sandboxes, err := docker.New(dctx, docker.Config{})
	if err != nil {
		k.printf("warning: connecting to Docker: %v; no sandbox is swept by this halt", err)
		return
	}
	k.sweep = func(ctx context.Context) (activity.SweepResult, error) {
		return activity.SweepOrphans(ctx, h.pool, sandboxes, 0, h.log)
	}
	k.remaining = func(ctx context.Context) (int, int, error) {
		unreleased, err := store.UnreleasedAttempts(ctx, h.pool, 0)
		if err != nil {
			return 0, 0, err
		}
		containers, err := sandboxes.CountSandboxes(ctx)
		return len(unreleased), containers, err
	}
}

// alerts opens the alert log and, if it can, the App. Without the App
// the alert is still written to the log, and its delivery is recorded
// there as failed.
func (h *haltConnector) alerts() (func(context.Context, alert.Alert) error, error) {
	log, err := alert.OpenLog(h.o.alertLog)
	if err != nil {
		return nil, err
	}
	h.closers = append(h.closers, func() { _ = log.Close() })
	var issues alert.Issues
	app, err := openAlertApp(h.o, h.keyFile)
	if err != nil {
		h.log.Warn("GitHub unavailable: alerts go to the local log only", "error", err)
		issues = unreachableIssues{err: fmt.Errorf("GitHub unavailable: %w", err)}
	} else {
		issues = app
	}
	return alerter{log: log, issues: issues}.raise, nil
}

func (h *haltConnector) close() {
	for i := len(h.closers) - 1; i >= 0; i-- {
		h.closers[i]()
	}
}

// runHalt needs only Postgres before the halt is recorded: the App key is
// optional, and Temporal, Docker and GitHub are connected afterwards.
func runHalt(ctx context.Context, o options, out io.Writer, log *slog.Logger) error {
	sec, err := readSecrets(databaseURLEnv)
	if err != nil {
		return err
	}
	keyFile, err := readOptionalSecret(github.AppKeyFileEnv)
	if err != nil {
		return err
	}
	pool, err := openPool(ctx, sec[databaseURLEnv])
	if err != nil {
		return err
	}
	defer pool.Close()
	h := &haltConnector{o: o, pool: pool, keyFile: keyFile, log: log}
	defer h.close()
	k := newKillSwitch(o, pool, out)
	k.connect = func(ctx context.Context) { h.connect(ctx, k) }
	return k.halt(ctx, o.reason)
}

func runResume(ctx context.Context, o options, out io.Writer, log *slog.Logger) error {
	sec, err := readSecrets(databaseURLEnv)
	if err != nil {
		return err
	}
	pool, err := openPool(ctx, sec[databaseURLEnv])
	if err != nil {
		return err
	}
	defer pool.Close()
	c, err := dialTemporal(ctx, o, connectTimeout, log)
	if err != nil {
		return err
	}
	defer c.Close()
	k := newKillSwitch(o, pool, out)
	k.runs = temporalRuns{c: c, namespace: o.namespace}
	return k.resume(ctx, o.reason, o.keep)
}

func runAlertTest(ctx context.Context, o options, out io.Writer) error {
	sec, err := readSecrets(databaseURLEnv, github.AppKeyFileEnv)
	if err != nil {
		return err
	}
	al, err := openAlerter(o, sec[github.AppKeyFileEnv])
	if err != nil {
		return err
	}
	defer func() { _ = al.log.Close() }()
	d, err := alert.Raise(ctx, al.log, al.issues, alert.Test(time.Now().UTC()))
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "test alert delivered: %s (new issue: %t)\n", d.URL, d.Created)
	return nil
}
