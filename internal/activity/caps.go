package activity

import (
	"context"

	"github.com/austmsk/conclave/internal/llm"
)

// CheckCaps reads the feature request's spend, settled and reserved,
// against the two platform caps, for /retry. It returns the first cap the
// spend is at or over, the feature request cap before the monthly one:
// at the cap no model call can be admitted, so a /retry would stop again
// at once. The caps are the worker's configuration as of startup; the
// owner raises one by editing deploy/router.yaml and restarting the
// worker.
//
// It reserves nothing and takes no lock, so it only decides whether to
// resume: every model call is still admitted by the router's own
// reservation. Idempotent retry: it only reads.
func (a *Activities) CheckCaps(ctx context.Context, featureRequestID string) (CapCheck, error) {
	res, err := a.checkCaps(ctx, featureRequestID)
	return res, Temporal(err)
}

func (a *Activities) checkCaps(ctx context.Context, featureRequestID string) (CapCheck, error) {
	if err := checkID("feature request", featureRequestID); err != nil {
		return CapCheck{}, err
	}
	sp, err := a.ledger.Spend(ctx, "", featureRequestID)
	if err != nil {
		return CapCheck{}, err
	}
	caps := a.d.RouterConfig.Budgets
	res := CapCheck{
		FeatureRequestTokens: sp.FeatureRequestTokens, FeatureRequestCap: caps.FeatureRequestTokens,
		MonthUSD: sp.MonthUSD, MonthCap: caps.MonthlyUSD,
	}
	switch {
	case sp.FeatureRequestTokens >= caps.FeatureRequestTokens:
		res.Exceeded = llm.BudgetFeatureRequest
	case sp.MonthUSD >= caps.MonthlyUSD:
		res.Exceeded = llm.BudgetMonth
	}
	return res, nil
}
