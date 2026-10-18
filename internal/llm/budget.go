package llm

import (
	"fmt"
	"time"
)

// BudgetKind names which FR-SEC9 cap refused a call.
type BudgetKind string

const (
	// BudgetAttempt is the scope's own token budget. Hitting it is an
	// attempt outcome (token_limit), not a platform refusal.
	BudgetAttempt BudgetKind = "attempt"
	// BudgetFeatureRequest is the token cap over every call made for one
	// feature request, in or out of an attempt.
	BudgetFeatureRequest BudgetKind = "feature_request"
	// BudgetMonth is the USD cap over every call in the current UTC
	// calendar month, across all repositories.
	BudgetMonth BudgetKind = "month"
)

// BudgetError is a call refused by one of the caps, before any provider was
// reached. It unwraps to ErrBudgetExceeded, so IsPermanent holds for it and
// errors.Is matches it; errors.As yields the kind. It is a value type:
// match it with a BudgetError target, not a *BudgetError.
//
// A refusal is not always the owner's spending. When recording a billed
// reply fails, its reservation is kept so the spend still counts, and until
// it expires (ReservationTTL) the next call can be refused on its account.
// A database fault can therefore surface as an attempt budget refusal.
type BudgetError struct {
	Kind BudgetKind
	// Detail is the spend and the cap, for logs and the status comment.
	Detail string
}

func (e BudgetError) Error() string {
	return fmt.Sprintf("%s budget: %s: %v", e.Kind, e.Detail, ErrBudgetExceeded)
}

// Unwrap lets errors.Is match ErrBudgetExceeded.
func (e BudgetError) Unwrap() error { return ErrBudgetExceeded }

// MaxCallDuration is the longest one provider call may run. Every adapter
// must bound a whole request by it, because a call that outlives its
// reservation stops counting against the caps while the provider is still
// generating. The Anthropic adapter's DefaultTimeout is this, and its
// constructor refuses an injected client without a timeout inside it.
const MaxCallDuration = 10 * time.Minute

// ReservationTTL is how long a reservation counts if it is never settled:
// the longest call plus a margin for settling it. The rate limiter is
// waited on before the reservation is taken, so its wait does not eat into
// this.
const ReservationTTL = MaxCallDuration + 5*time.Minute

// Spend is what the ledger holds against each cap when a call is about to
// be made: settled spend plus live reservations. AttemptTokens is zero for
// a call made outside an attempt.
type Spend struct {
	AttemptTokens        int
	FeatureRequestTokens int
	MonthUSD             float64
}

// Claim is what one call reserves: tokens against the attempt and feature
// request caps, and their price against the monthly cap.
type Claim struct {
	Tokens  int
	CostUSD float64
}

// ask is one call's request on the budgets: its estimated input, the output
// it would like, and the caps that apply to it.
type ask struct {
	model      ModelConfig
	input      int
	wantOutput int
	// attemptBudget is the scope's token budget, or zero for a scope
	// outside any attempt, which has none.
	attemptBudget int
	budgets       Budgets
}

// decide grants the call as much output as every cap leaves room for, or
// refuses it naming the first cap it would pass. It runs while the ledger
// holds the budget lock, so it does arithmetic only.
//
// The token caps shrink the output granted, so a call near a cap is sent
// with a smaller max_tokens rather than refused. The monthly cap does not:
// it is in dollars, and a call that does not fit is refused.
func (a ask) decide(sp Spend) (int, Claim, error) {
	output := a.wantOutput
	var err error
	if a.attemptBudget > 0 {
		output, err = fit(BudgetAttempt, a.input, output, a.attemptBudget-sp.AttemptTokens, a.attemptBudget)
		if err != nil {
			return 0, Claim{}, err
		}
	}
	frCap := a.budgets.FeatureRequestTokens
	output, err = fit(BudgetFeatureRequest, a.input, output, frCap-sp.FeatureRequestTokens, frCap)
	if err != nil {
		return 0, Claim{}, err
	}
	c := Claim{Tokens: a.input + output, CostUSD: claimUSD(a.model, a.input, output)}
	// Written as a negation so that a NaN anywhere refuses the call.
	if !(sp.MonthUSD+c.CostUSD <= a.budgets.MonthlyUSD) {
		return 0, Claim{}, BudgetError{Kind: BudgetMonth, Detail: fmt.Sprintf(
			"$%.6f spent or reserved of $%.2f this month, call may cost $%.6f",
			sp.MonthUSD, a.budgets.MonthlyUSD, c.CostUSD)}
	}
	return output, c, nil
}

// fit grants up to want output tokens alongside input within left tokens.
// The call is refused when the input alone does not fit, or when output is
// wanted and no room for it is left.
func fit(kind BudgetKind, input, want, left, limit int) (int, error) {
	output := min(want, left-input)
	if left <= 0 || input > left || (want > 0 && output <= 0) {
		return 0, BudgetError{Kind: kind, Detail: fmt.Sprintf(
			"%d tokens spent or reserved of %d, call needs about %d", limit-left, limit, input)}
	}
	return output, nil
}

// claimUSD is the most a call of input and output tokens can cost on m.
// Input is priced at the dearest of the input and cache rates, because the
// estimate cannot say how much of it the provider will bill as a cache
// write, which can cost more than plain input.
func claimUSD(m ModelConfig, input, output int) float64 {
	in := max(m.InputUSDPerMTok, m.CacheWriteUSDPerMTok, m.CacheReadUSDPerMTok)
	return (float64(input)*in + float64(output)*m.OutputUSDPerMTok) / 1e6
}
