package store_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/austmsk/conclave/internal/contracts"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/store"
)

// spendNow is what the ledger holds against each cap for the attempt and
// feature request right now.
func spendNow(t *testing.T, ctx context.Context, ledger *store.CostLedger, attempt, feature string) llm.Spend {
	t.Helper()
	sp, err := ledger.Spend(ctx, attempt, feature)
	if err != nil {
		t.Fatalf("reading spend: %v", err)
	}
	return sp
}

// TestAttributed: an attempt is attributed only to the feature request its
// task's plan belongs to, and a feature request only to its repository.
func TestAttributed(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	other := seedRepo(t, ctx, tx, "austmsk/other")
	ledger := store.NewCostLedger(tx)

	cases := []struct {
		name                   string
		repo, feature, attempt string
		want                   bool
	}{
		{"attempt in its feature request", s.repo, s.feature, s.attempt, true},
		{"feature request alone", s.repo, s.feature, "", true},
		{"attempt of another feature request", s.repo, s.feature, other.attempt, false},
		{"feature request of another repository", s.repo, other.feature, "", false},
		{"attempt under the wrong repository", other.repo, s.feature, s.attempt, false},
		{"unknown attempt", s.repo, s.feature, "00000000-0000-4000-8000-000000000000", false},
		{"attempt not a UUID", s.repo, s.feature, "attempt-1", false},
		{"feature request not a UUID", s.repo, "issue-12", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ledger.Attributed(ctx, tc.repo, tc.feature, tc.attempt)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("Attributed = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReserveSumsSpend: decide sees settled and live reserved spend, each
// against the caps it belongs to, and an expired reservation nowhere.
func TestReserveSumsSpend(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	sibling, err := insertInState(ctx, tx, s, contracts.EntityAttempt, "running")
	if err != nil {
		t.Fatalf("seeding second attempt: %v", err)
	}
	elsewhere := seedRepo(t, ctx, tx, "austmsk/elsewhere")
	ledger := store.NewCostLedger(tx)
	base := spendNow(t, ctx, ledger, s.attempt, s.feature)

	settle := func(ev contracts.CostEvent) {
		t.Helper()
		ev.OccurredAt = time.Time{} // this month, by the database clock
		if err := ledger.Settle(ctx, "", ev); err != nil {
			t.Fatalf("settling %s: %v", ev.IdempotencyKey, err)
		}
	}
	settle(costEvent(s, "a/1", 100, 10)) // this attempt: $0.01
	siblingEvent := costEvent(s, "b/1", 1000, 0)
	siblingEvent.AttemptID = sibling
	settle(siblingEvent)
	analysis := costEvent(s, "c/1", 5000, 0) // the feature request, outside any attempt
	analysis.AttemptID = ""
	settle(analysis)
	settle(costEvent(elsewhere, "d/1", 70000, 0))

	reserve := func(attempt, feature string, tokens int, usd float64) {
		t.Helper()
		if _, err := ledger.Reserve(ctx, attempt, feature, time.Minute, func(llm.Spend) (llm.Claim, error) {
			return llm.Claim{Tokens: tokens, CostUSD: usd}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	reserve(s.attempt, s.feature, 20, 0.5)
	reserve(sibling, s.feature, 300, 0.25)
	reserve(elsewhere.attempt, elsewhere.feature, 900000, 1)
	// An expired reservation, as a dead process leaves behind.
	_, err = tx.Exec(ctx, `INSERT INTO budget_reservations (attempt_id, feature_request_id, tokens, cost_usd, expires_at)
		VALUES ($1, $2, 1000000, 100, statement_timestamp() - interval '1 microsecond')`, s.attempt, s.feature)
	if err != nil {
		t.Fatalf("inserting expired reservation: %v", err)
	}

	got := spendNow(t, ctx, ledger, s.attempt, s.feature)
	want := llm.Spend{
		AttemptTokens:        base.AttemptTokens + 110 + 20,
		FeatureRequestTokens: base.FeatureRequestTokens + 110 + 1000 + 5000 + 20 + 300,
		MonthUSD:             base.MonthUSD + 4*0.01 + 0.5 + 0.25 + 1,
	}
	if got.AttemptTokens != want.AttemptTokens || got.FeatureRequestTokens != want.FeatureRequestTokens ||
		math.Abs(got.MonthUSD-want.MonthUSD) > 1e-9 {
		t.Fatalf("spend %+v, want %+v", got, want)
	}
}

// TestReservationExpiresOnDatabaseClock: a reservation's expiry is the
// database's time plus the TTL, not the worker's, and it counts until then.
func TestReservationExpiresOnDatabaseClock(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	ledger := store.NewCostLedger(tx)
	const ttl = 15 * time.Minute
	id, err := ledger.Reserve(ctx, s.attempt, s.feature, ttl, func(llm.Spend) (llm.Claim, error) {
		return llm.Claim{Tokens: 7}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var seconds float64
	err = tx.QueryRow(ctx, `SELECT extract(epoch FROM expires_at - statement_timestamp())::float8
		FROM budget_reservations WHERE id = $1`, id).Scan(&seconds)
	if err != nil {
		t.Fatal(err)
	}
	left := time.Duration(seconds * float64(time.Second))
	if left < ttl-time.Minute || left > ttl {
		t.Fatalf("reservation expires in %s, want just under %s", left, ttl)
	}
	if _, err := ledger.Reserve(ctx, s.attempt, s.feature, 0, nil); err == nil {
		t.Fatal("reserved with a TTL of zero")
	}
}

// TestMonthIsUTC puts spend on each side of the start of the UTC month and
// checks only the later one counts. It runs in sessions far from UTC on
// either side, where truncating to the month in the session's time zone
// would move the boundary by half a day.
func TestMonthIsUTC(t *testing.T) {
	for _, zone := range []string{"UTC", "Pacific/Kiritimati", "Etc/GMT+12"} {
		t.Run(zone, func(t *testing.T) { monthCase(t, zone) })
	}
}

func monthCase(t *testing.T, zone string) {
	ctx, tx := testTx(t)
	if _, err := tx.Exec(ctx, `SELECT set_config('TimeZone', $1, true)`, zone); err != nil {
		t.Fatal(err)
	}
	s := seed(t, ctx, tx)
	ledger := store.NewCostLedger(tx)
	var monthStart time.Time
	err := tx.QueryRow(ctx, `SELECT date_trunc('month', statement_timestamp() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`).Scan(&monthStart)
	if err != nil {
		t.Fatal(err)
	}
	if monthStart.UTC().Day() != 1 || monthStart.UTC().Hour() != 0 {
		t.Fatalf("month start %s is not midnight UTC on the first", monthStart.UTC())
	}
	base := spendNow(t, ctx, ledger, s.attempt, s.feature).MonthUSD

	last := costEvent(s, "last-month", 1, 1)
	last.CostUSD, last.OccurredAt = 1, monthStart.Add(-time.Microsecond)
	this := costEvent(s, "this-month", 1, 1)
	this.CostUSD, this.OccurredAt = 0.25, monthStart
	for _, ev := range []contracts.CostEvent{last, this} {
		if err := ledger.Settle(ctx, "", ev); err != nil {
			t.Fatal(err)
		}
	}
	if got := spendNow(t, ctx, ledger, s.attempt, s.feature).MonthUSD - base; math.Abs(got-0.25) > 1e-9 {
		t.Fatalf("month spend rose by %v, want 0.25: only the event at the UTC month start counts", got)
	}
}

// TestReserveRefusalReservesNothing: decide's error comes back unchanged,
// so errors.As still finds a BudgetError, and no reservation is left.
func TestReserveRefusalReservesNothing(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	ledger := store.NewCostLedger(tx)
	refusal := llm.BudgetError{Kind: llm.BudgetFeatureRequest, Detail: "test"}
	_, err := ledger.Reserve(ctx, s.attempt, s.feature, time.Minute, func(llm.Spend) (llm.Claim, error) {
		return llm.Claim{}, refusal
	})
	var be llm.BudgetError
	if !errors.As(err, &be) || be != refusal {
		t.Fatalf("got %v, want the refusal unchanged", err)
	}
	if n := countRows(t, ctx, tx, `SELECT count(*) FROM budget_reservations`); n != 0 {
		t.Fatalf("%d reservations after a refusal, want 0", n)
	}
}

// TestReserveRefusesBadClaims: a claim that is negative or not a number
// would make room under a cap or defeat the comparison with it.
func TestReserveRefusesBadClaims(t *testing.T) {
	claims := []llm.Claim{
		{Tokens: -1},
		{CostUSD: -0.01},
		{CostUSD: math.NaN()},
		{CostUSD: math.Inf(1)},
	}
	for _, c := range claims {
		t.Run(fmt.Sprintf("%+v", c), func(t *testing.T) {
			ctx, tx := testTx(t)
			s := seed(t, ctx, tx)
			_, err := store.NewCostLedger(tx).Reserve(ctx, s.attempt, s.feature, time.Minute, func(llm.Spend) (llm.Claim, error) {
				return c, nil
			})
			if err == nil {
				t.Fatalf("reserved %+v", c)
			}
		})
	}
}

// TestAppRoleReservationGrants: the application role claims and releases
// reservations, and cannot change one. A reservation that could be
// updated could be shrunk after it was checked against the caps.
func TestAppRoleReservationGrants(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	if _, err := store.NewCostLedger(tx).Reserve(ctx, s.attempt, s.feature, time.Minute, func(llm.Spend) (llm.Claim, error) {
		return llm.Claim{Tokens: 100, CostUSD: 1}, nil
	}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, sql, want string
	}{
		{"read", `SELECT count(*) FROM budget_reservations`, ""},
		{"insert", `INSERT INTO budget_reservations (feature_request_id, tokens, cost_usd, expires_at)
			VALUES ('` + s.feature + `', 1, 0, now())`, ""},
		{"shrink", `UPDATE budget_reservations SET tokens = 0`, permissionDenied},
		{"expire", `UPDATE budget_reservations SET expires_at = now()`, permissionDenied},
		{"release", `DELETE FROM budget_reservations`, ""},
		{"truncate", `TRUNCATE budget_reservations`, permissionDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := savepoint(t, ctx, tx)
			_, err := sp.Exec(ctx, tc.sql)
			if got := sqlState(err); got != tc.want {
				t.Fatalf("got error %v (SQLSTATE %q), want SQLSTATE %q", err, got, tc.want)
			}
		})
	}
}

// seedRepo seeds a second chain of records under a repository of its own,
// since the seed repository's name is unique.
func seedRepo(t *testing.T, ctx context.Context, tx pgx.Tx, name string) ids {
	t.Helper()
	var s ids
	err := tx.QueryRow(ctx, `INSERT INTO repos (full_name, tier, default_branch)
		VALUES ($1, 'public', 'main') RETURNING id::text`, name).Scan(&s.repo)
	if err != nil {
		t.Fatalf("seeding repo: %v", err)
	}
	steps := []struct {
		dst   *string
		kind  contracts.EntityKind
		state string
	}{
		{&s.feature, contracts.EntityFeatureRequest, "planning"},
		{&s.plan, contracts.EntityPlan, "proposed"},
		{&s.task, contracts.EntityTask, "running"},
		{&s.attempt, contracts.EntityAttempt, "running"},
	}
	for _, st := range steps {
		id, err := insertInState(ctx, tx, s, st.kind, st.state)
		if err != nil {
			t.Fatalf("seeding %s: %v", st.kind, err)
		}
		*st.dst = id
	}
	return s
}

// TestReserveSweepsExpired: taking a reservation deletes the ones that have
// expired, so the table every reservation sums holds only calls in flight.
func TestReserveSweepsExpired(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	_, err := tx.Exec(ctx, `INSERT INTO budget_reservations (attempt_id, feature_request_id, tokens, cost_usd, expires_at)
		VALUES ($1, $2, 5, 0, statement_timestamp() - interval '1 microsecond')`, s.attempt, s.feature)
	if err != nil {
		t.Fatal(err)
	}
	live := reserveTokens(t, ctx, store.NewCostLedger(tx), s, 7)
	if n := countRows(t, ctx, tx, `SELECT count(*) FROM budget_reservations`); n != 1 {
		t.Fatalf("%d reservations, want only the live one", n)
	}
	if n := countRows(t, ctx, tx, `SELECT count(*) FROM budget_reservations WHERE id = $1`, live); n != 1 {
		t.Fatal("the live reservation was deleted")
	}
}

// TestReserveRefusesOtherIsolation: given a transaction it can only nest
// inside, Reserve checks the level rather than trusting it. At REPEATABLE
// READ a reservation waiting on the lock would read spend from before the
// previous holder committed.
func TestReserveRefusesOtherIsolation(t *testing.T) {
	for _, level := range []string{"REPEATABLE READ", "SERIALIZABLE"} {
		t.Run(level, func(t *testing.T) {
			ctx := requireDB(t)
			tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.TxIsoLevel(strings.ToLower(level))})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tx.Rollback(ctx) })
			s := seed(t, ctx, tx)
			_, err = store.NewCostLedger(tx).Reserve(ctx, s.attempt, s.feature, time.Minute, func(llm.Spend) (llm.Claim, error) {
				return llm.Claim{Tokens: 1}, nil
			})
			if err == nil || !strings.Contains(err.Error(), "read committed") {
				t.Fatalf("got %v, want a refusal naming read committed", err)
			}
		})
	}
}

func TestSettleWithoutReservation(t *testing.T) {
	ctx, tx := testTx(t)
	s := seed(t, ctx, tx)
	ledger := store.NewCostLedger(tx)
	held := reserveTokens(t, ctx, ledger, s, 500)
	if err := ledger.Settle(ctx, "", costEvent(s, "unreserved", 10, 5)); err != nil {
		t.Fatal(err)
	}
	if sp := spendNow(t, ctx, ledger, s.attempt, s.feature); sp.AttemptTokens != 515 {
		t.Fatalf("attempt spend %d, want the 15 recorded plus the 500 still reserved under %s", sp.AttemptTokens, held)
	}
}
