package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies every pending migration. It is safe to call on an
// up-to-date database, which makes it usable at startup.
func Migrate(ctx context.Context, db *sql.DB) error {
	p, err := provider(db)
	if err != nil {
		return err
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}
	return nil
}

// Rollback reverts every applied migration, leaving an empty schema. It
// exists so the down migrations are exercised, not for production use.
func Rollback(ctx context.Context, db *sql.DB) error {
	p, err := provider(db)
	if err != nil {
		return err
	}
	if _, err := p.DownTo(ctx, 0); err != nil {
		return fmt.Errorf("rolling back migrations: %w", err)
	}
	return nil
}

func provider(db *sql.DB) (*goose.Provider, error) {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("opening embedded migrations: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		return nil, fmt.Errorf("creating migration provider: %w", err)
	}
	return p, nil
}
