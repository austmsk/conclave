package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/austmsk/conclave/internal/github"
)

// databaseURLEnv names the database secret, as for the worker.
const databaseURLEnv = "CONCLAVE_DATABASE_URL"

// defaultDrain is how long halt waits for open workflows to close. It
// matches the worker's halt grace, so cancellations land while the
// workers still run.
const defaultDrain = 3 * time.Minute

// listPoll is the wait between listings while halt drains.
const listPoll = time.Second

const usage = `usage: conclave <command> [flags]

commands:
  halt -reason <text>          the kill switch: cancel all work and stop the workers
  resume -reason <text> [-keep] clear the halt; cancels runs started during it unless -keep
  alert-test                   raise a test alert through the log and the alerts repository
  report [-from T] [-to T]     print the trial report (times in RFC 3339)`

// options are every subcommand's flags; each subcommand reads its own.
type options struct {
	githubConfig string
	alertLog     string
	temporalAddr string
	namespace    string
	drain        time.Duration
	reason       string
	keep         bool
	from, to     string
}

func main() {
	if err := runCLI(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "conclave:", err)
		os.Exit(1)
	}
}

func runCLI(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	o, err := parseFlags(args[0], args[1:])
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{ReplaceAttr: github.RedactAttr}))
	switch args[0] {
	case "halt":
		return runHalt(ctx, o, out, log)
	case "resume":
		return runResume(ctx, o, out, log)
	case "alert-test":
		return runAlertTest(ctx, o, out)
	default:
		return runReport(ctx, o, out)
	}
}

// parseFlags parses one subcommand's flags and checks the ones it needs.
func parseFlags(cmd string, args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.githubConfig, "github-config", "deploy/github.yaml", "GitHub App configuration")
	fs.StringVar(&o.temporalAddr, "temporal-address", client.DefaultHostPort, "Temporal frontend address")
	fs.StringVar(&o.namespace, "temporal-namespace", client.DefaultNamespace, "Temporal namespace")
	switch cmd {
	case "halt":
		fs.StringVar(&o.alertLog, "alert-log", "", "local append-only alert log (required)")
		fs.DurationVar(&o.drain, "drain", defaultDrain, "how long to wait for open workflows to close")
		fs.StringVar(&o.reason, "reason", "", "why (required)")
	case "resume":
		fs.StringVar(&o.reason, "reason", "", "why (required)")
		fs.BoolVar(&o.keep, "keep", false, "keep runs started during the halt instead of cancelling them")
	case "alert-test":
		fs.StringVar(&o.alertLog, "alert-log", "", "local append-only alert log (required)")
	case "report":
		fs.StringVar(&o.from, "from", "", "first trial's start, RFC 3339 (default: all)")
		fs.StringVar(&o.to, "to", "", "last trial's start, RFC 3339 (default: now)")
	default:
		return options{}, fmt.Errorf("unknown command %q\n%s", cmd, usage)
	}
	if err := fs.Parse(args); err != nil {
		return options{}, fmt.Errorf("%s: %w\n%s", cmd, err, usage)
	}
	return o, checkOptions(cmd, o)
}

func checkOptions(cmd string, o options) error {
	switch {
	case (cmd == "halt" || cmd == "resume") && o.reason == "":
		return fmt.Errorf("%s needs -reason", cmd)
	case (cmd == "halt" || cmd == "alert-test") && o.alertLog == "":
		return fmt.Errorf("%s needs -alert-log", cmd)
	case cmd == "halt" && o.drain < 0:
		return errors.New("-drain must not be negative")
	}
	return nil
}

// readSecrets reads and unsets each named variable. A missing one stops
// the command by name, never by value.
func readSecrets(names ...string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	for _, name := range names {
		v := os.Getenv(name)
		if err := os.Unsetenv(name); err != nil {
			return nil, fmt.Errorf("unsetting %s: %w", name, err)
		}
		if v == "" {
			return nil, fmt.Errorf("%s is not set", name)
		}
		out[name] = v
	}
	return out, nil
}
