package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/austmsk/conclave/internal/contracts"
)

// ErrInvalidID means an ID given to the store is not a UUID. Every key in
// the schema is one; without this, a malformed ID surfaces as Postgres's
// "invalid input syntax for type uuid", which reads like a database fault
// and would be retried as one.
var ErrInvalidID = errors.New("ID is not a UUID")

// checkUUID parses v with pgx's own UUID type before it reaches the
// database, because a statement Postgres refuses aborts the caller's
// transaction. pgx is laxer than Postgres in one way: in the 36-character
// form it does not check that the separators are dashes. Such an ID gets
// past this, is refused by Postgres, and isInvalidText still reports it as
// ErrInvalidID, but a caller using a pgx.Tx loses the transaction. IDs the
// platform handles are written by Postgres itself, so the case needs a
// hand-edited ID.
func checkUUID(field, v string) error {
	var u pgtype.UUID
	if err := u.Scan(v); err != nil {
		return fmt.Errorf("%s %q: %w", field, v, ErrInvalidID)
	}
	return nil
}

// sqlStateInvalidText is invalid_text_representation, which Postgres
// raises when a parameter does not parse as its column's type. The only
// parameters these queries convert from text are UUIDs.
const sqlStateInvalidText = "22P02"

func isInvalidText(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == sqlStateInvalidText
}

// RepoDirectory reads repository records. It implements llm.RepoDirectory:
// the router reads a repository's record on every request and refuses it
// when the record's tier disagrees with the owner's configuration, which
// stays authoritative because the application role can write this table
// (FR-SEC12).
type RepoDirectory struct {
	db DBTX
}

// NewRepoDirectory returns a directory over db.
func NewRepoDirectory(db DBTX) *RepoDirectory {
	return &RepoDirectory{db: db}
}

// Repo returns the repository with the given ID. An ID no repository has,
// including one that is not a UUID and so cannot be any repository's,
// reports found false with a nil error.
func (d *RepoDirectory) Repo(ctx context.Context, id string) (contracts.Repo, bool, error) {
	if checkUUID("repository ID", id) != nil {
		return contracts.Repo{}, false, nil
	}
	const query = `SELECT id::text, full_name, tier, default_branch FROM repos WHERE id = $1`
	var r contracts.Repo
	err := d.db.QueryRow(ctx, query, id).Scan(&r.ID, &r.FullName, &r.Tier, &r.DefaultBranch)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return contracts.Repo{}, false, nil
	}
	if err != nil {
		return contracts.Repo{}, false, fmt.Errorf("reading repository %s: %w", id, err)
	}
	return r, true, nil
}
