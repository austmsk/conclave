package main

import (
	"context"
	"log/slog"

	"github.com/austmsk/conclave/internal/store"
)

// checkLogin refuses to start as a login the worker's grants were not
// written for: the schema owner, a superuser, an operator (which could
// lift a halt), or anything outside conclave_app (decision 0002). The
// login's name is logged, never its password, so a run's rows can be
// matched to the worker that wrote them.
func checkLogin(ctx context.Context, db store.DBTX, log *slog.Logger) error {
	l, err := store.CurrentLogin(ctx, db)
	if err != nil {
		return err
	}
	if err := l.CheckWorker(); err != nil {
		return err
	}
	log.Info("database login", "login", l.Name)
	return nil
}
