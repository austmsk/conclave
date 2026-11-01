package store

import (
	"context"
	_ "embed"
	"fmt"
	"time"
)

// trialReportQuery is the one trial-report query: trial_report.sql
// documents each column.
//
//go:embed trial_report.sql
var trialReportQuery string

// Feature request states the summary counts.
const (
	stateDelivered = "delivered"
	stateCancelled = "cancelled"
)

// TrialRow is one trial: one feature request, which one label started.
type TrialRow struct {
	FeatureRequestID, Repo string
	Issue                  int
	State                  string
	Attempts               int
	InputTokens            int64
	OutputTokens           int64
	CostUSD                float64
	// AgentTime is the time finished attempts held a sandbox.
	AgentTime time.Duration
	// ApproveToDelivered is the time from the plan's approval to delivery,
	// zero when the trial was not delivered.
	ApproveToDelivered time.Duration
}

// TrialSummary is the trial report's three figures and the counts behind
// them.
type TrialSummary struct {
	Trials, Delivered, Cancelled int
	// ResolveRate is Delivered / Trials. Cancelled trials count as trials:
	// whether the criterion excludes them is decided by whoever reads it,
	// so Cancelled is reported beside it.
	ResolveRate float64
	// CostPerResolvedUSD is the spend of every trial divided by the number
	// delivered: what a resolved task really cost, failed attempts and
	// trials included.
	CostPerResolvedUSD float64
	// TimePerResolved is the mean ApproveToDelivered over delivered
	// trials.
	TimePerResolved time.Duration
}

// TrialReport is the rows and their summary.
type TrialReport struct {
	Rows    []TrialRow
	Summary TrialSummary
}

// ReadTrialReport runs the trial-report query over feature requests
// created in [from, to) and summarizes the rows. It only reads, and the
// application role may run it.
func ReadTrialReport(ctx context.Context, db QueryTxDB, from, to time.Time) (TrialReport, error) {
	if !from.Before(to) {
		return TrialReport{}, fmt.Errorf("trial report: window %s to %s is empty", from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	rows, err := db.Query(ctx, trialReportQuery, from, to)
	if err != nil {
		return TrialReport{}, fmt.Errorf("running the trial report: %w", err)
	}
	defer rows.Close()
	var rep TrialReport
	for rows.Next() {
		var r TrialRow
		var agent, delivered float64
		if err := rows.Scan(&r.FeatureRequestID, &r.Repo, &r.Issue, &r.State, &r.Attempts,
			&r.InputTokens, &r.OutputTokens, &r.CostUSD, &agent, &delivered); err != nil {
			return TrialReport{}, fmt.Errorf("reading a trial report row: %w", err)
		}
		r.AgentTime, r.ApproveToDelivered = seconds(agent), seconds(delivered)
		rep.Rows = append(rep.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return TrialReport{}, fmt.Errorf("reading the trial report: %w", err)
	}
	rep.Summary = summarize(rep.Rows)
	return rep, nil
}

func seconds(s float64) time.Duration {
	return time.Duration(s * float64(time.Second)).Round(time.Millisecond)
}

func summarize(rows []TrialRow) TrialSummary {
	s := TrialSummary{Trials: len(rows)}
	var cost float64
	var deliveredTime time.Duration
	for _, r := range rows {
		cost += r.CostUSD
		switch r.State {
		case stateDelivered:
			s.Delivered++
			deliveredTime += r.ApproveToDelivered
		case stateCancelled:
			s.Cancelled++
		}
	}
	if s.Trials > 0 {
		s.ResolveRate = float64(s.Delivered) / float64(s.Trials)
	}
	if s.Delivered > 0 {
		s.CostPerResolvedUSD = cost / float64(s.Delivered)
		s.TimePerResolved = deliveredTime / time.Duration(s.Delivered)
	}
	return s
}
