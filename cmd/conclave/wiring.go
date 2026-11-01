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
	log *alert.Log
	app *github.App
}

func (a alerter) raise(ctx context.Context, al alert.Alert) error {
	_, err := alert.Raise(ctx, a.log, a.app, al)
	return err
}

// openAlerter loads the GitHub configuration, which must name an alerts
// repository, the App key, and the alert log.
func openAlerter(o options, keyFile string) (alerter, error) {
	var cfg github.Config
	if _, err := deployfile.Load(o.githubConfig, func(r io.Reader) (err error) { cfg, err = github.LoadConfig(r); return err }); err != nil {
		return alerter{}, err
	}
	if err := cfg.RequireAlerts(); err != nil {
		return alerter{}, err
	}
	key, err := github.LoadAppKey(keyFile)
	if err != nil {
		return alerter{}, err
	}
	app, err := github.NewApp(cfg, key, nil)
	if err != nil {
		return alerter{}, err
	}
	log, err := alert.OpenLog(o.alertLog)
	if err != nil {
		return alerter{}, err
	}
	return alerter{log: log, app: app}, nil
}

func openPool(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connecting to Postgres: %w", err)
	}
	return pool, nil
}

func dialTemporal(o options, log *slog.Logger) (client.Client, error) {
	c, err := client.Dial(client.Options{HostPort: o.temporalAddr, Namespace: o.namespace, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("connecting to Temporal at %s: %w", o.temporalAddr, err)
	}
	return c, nil
}

// newKillSwitch wires the kill switch to Postgres and Temporal. sandboxes
// and alerts are nil for resume, which neither sweeps nor alerts.
func newKillSwitch(o options, pool *pgxpool.Pool, c client.Client, sandboxes *docker.Provider, al alerter, out io.Writer, log *slog.Logger) *killSwitch {
	return &killSwitch{
		setHalt: func(ctx context.Context, r string) (store.HaltState, error) {
			return store.SetHalt(ctx, pool, r, store.ActorCLI)
		},
		clearHalt: func(ctx context.Context, r string) (store.HaltState, error) {
			return store.ClearHalt(ctx, pool, r, store.ActorCLI)
		},
		currentHalt: func(ctx context.Context) (store.HaltState, error) { return store.CurrentHalt(ctx, pool) },
		runs:        temporalRuns{c: c, namespace: o.namespace},
		sweep: func(ctx context.Context) (activity.SweepResult, error) {
			return activity.SweepOrphans(ctx, pool, sandboxes, 0, log)
		},
		remaining: func(ctx context.Context) (int, int, error) {
			unreleased, err := store.UnreleasedAttempts(ctx, pool, 0)
			if err != nil {
				return 0, 0, err
			}
			containers, err := sandboxes.CountSandboxes(ctx)
			return len(unreleased), containers, err
		},
		raise: al.raise,
		drain: o.drain, poll: listPoll, now: time.Now, sleep: sleepCtx, out: out,
	}
}

func runHalt(ctx context.Context, o options, out io.Writer, log *slog.Logger) error {
	sec, err := readSecrets(databaseURLEnv, github.AppKeyFileEnv)
	if err != nil {
		return err
	}
	al, err := openAlerter(o, sec[github.AppKeyFileEnv])
	if err != nil {
		return err
	}
	defer func() { _ = al.log.Close() }()
	pool, err := openPool(ctx, sec[databaseURLEnv])
	if err != nil {
		return err
	}
	defer pool.Close()
	c, err := dialTemporal(o, log)
	if err != nil {
		return err
	}
	defer c.Close()
	sandboxes, err := docker.New(ctx, docker.Config{})
	if err != nil {
		return fmt.Errorf("connecting to Docker: %w", err)
	}
	return newKillSwitch(o, pool, c, sandboxes, al, out, log).halt(ctx, o.reason)
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
	c, err := dialTemporal(o, log)
	if err != nil {
		return err
	}
	defer c.Close()
	return newKillSwitch(o, pool, c, nil, alerter{}, out, log).resume(ctx, o.reason, o.keep)
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
	d, err := alert.Raise(ctx, al.log, al.app, alert.Test(time.Now().UTC()))
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "test alert delivered: %s (new issue: %t)\n", d.URL, d.Created)
	return nil
}
