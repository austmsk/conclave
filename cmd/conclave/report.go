package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/austmsk/conclave/internal/store"
)

// runReport prints the trial report: one row per feature request started
// in the window, then the summary Milestone 2's report generator starts
// from.
func runReport(ctx context.Context, o options, out io.Writer) error {
	from, to, err := reportWindow(o.from, o.to, time.Now())
	if err != nil {
		return err
	}
	sec, err := readSecrets(databaseURLEnv)
	if err != nil {
		return err
	}
	pool, err := openPool(ctx, sec[databaseURLEnv])
	if err != nil {
		return err
	}
	defer pool.Close()
	rep, err := store.ReadTrialReport(ctx, pool, from, to)
	if err != nil {
		return err
	}
	return printReport(out, rep)
}

// reportWindow parses -from and -to; an empty -from is the beginning of
// time and an empty -to is now.
func reportWindow(fromFlag, toFlag string, now time.Time) (from, to time.Time, err error) {
	to = now
	if fromFlag != "" {
		if from, err = time.Parse(time.RFC3339, fromFlag); err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("-from: %w", err)
		}
	}
	if toFlag != "" {
		if to, err = time.Parse(time.RFC3339, toFlag); err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("-to: %w", err)
		}
	}
	if to.Before(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("-to %s is before -from %s", to.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	return from, to, nil
}

func printReport(out io.Writer, rep store.TrialReport) error {
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "repository\tissue\tstate\tattempts\tinput tokens\toutput tokens\tcost USD\tagent time\tapprove to delivered")
	for _, r := range rep.Rows {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%d\t%d\t%.4f\t%s\t%s\n", r.Repo, r.Issue, r.State, r.Attempts,
			r.InputTokens, r.OutputTokens, r.CostUSD, r.AgentTime.Round(time.Second), r.ApproveToDelivered.Round(time.Second))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("printing the report: %w", err)
	}
	s := rep.Summary
	_, err := fmt.Fprintf(out, "\ntrials %d, delivered %d, cancelled %d; resolve rate %.0f%%; cost per resolved task $%.4f; time per resolved task %s\n",
		s.Trials, s.Delivered, s.Cancelled, 100*s.ResolveRate, s.CostPerResolvedUSD, s.TimePerResolved.Round(time.Second))
	return err
}
