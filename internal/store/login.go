package store

import (
	"context"
	"errors"
	"fmt"
)

// RoleApp and RoleOperator are the database roles the worker's and the
// CLI's logins are members of (migrations 00004 and 00011).
const (
	RoleApp      = "conclave_app"
	RoleOperator = "conclave_operator"
)

// ErrWrongLogin means a process connected as a login its grants were not
// meant for. Every refusal names the login, never its password.
var ErrWrongLogin = errors.New("wrong database login")

// Login is the login a connection authenticated as, and what it can
// become. Name is session_user, which SET ROLE does not change.
type Login struct {
	Name string
	// Superuser and Owner are logins no application process may hold: the
	// table owner, or a member of it, can disable the audit triggers and
	// rewrite append-only rows, and a superuser bypasses every grant.
	Superuser bool
	Owner     bool
	// App and Operator are membership of RoleApp and RoleOperator. An
	// operator login is an app member too.
	App      bool
	Operator bool
}

// CurrentLogin reads the connection's login. The owner is whoever owns
// halt_events, which the migrations create along with every other table.
func CurrentLogin(ctx context.Context, db DBTX) (Login, error) {
	var l Login
	err := db.QueryRow(ctx, `
		SELECT session_user::text,
		       (SELECT rolsuper FROM pg_roles WHERE rolname = session_user),
		       pg_has_role(session_user, (SELECT tableowner FROM pg_tables
		                                  WHERE schemaname = 'public' AND tablename = 'halt_events'), 'MEMBER'),
		       pg_has_role(session_user, $1, 'MEMBER'),
		       pg_has_role(session_user, $2, 'MEMBER')`, RoleApp, RoleOperator).
		Scan(&l.Name, &l.Superuser, &l.Owner, &l.App, &l.Operator)
	if err != nil {
		return Login{}, fmt.Errorf("reading the database login: %w", err)
	}
	return l, nil
}

// CheckWorker refuses a login a worker must not run as: the owner, a
// superuser, anything outside RoleApp, or an operator, which could lift a
// halt.
func (l Login) CheckWorker() error {
	if err := l.checkNotOwner(); err != nil {
		return err
	}
	switch {
	case l.Operator:
		return fmt.Errorf("%w: %s is a %s member and could lift a halt; give the worker a login in %s only", ErrWrongLogin, l.Name, RoleOperator, RoleApp)
	case !l.App:
		return fmt.Errorf("%w: %s is not a %s member", ErrWrongLogin, l.Name, RoleApp)
	}
	return nil
}

// CheckOperator refuses a login the CLI must not run as: the owner, a
// superuser, or anything outside RoleOperator.
func (l Login) CheckOperator() error {
	if err := l.checkNotOwner(); err != nil {
		return err
	}
	if !l.Operator {
		return fmt.Errorf("%w: %s is not a %s member", ErrWrongLogin, l.Name, RoleOperator)
	}
	return nil
}

func (l Login) checkNotOwner() error {
	switch {
	case l.Superuser:
		return fmt.Errorf("%w: %s is a superuser, which bypasses every grant", ErrWrongLogin, l.Name)
	case l.Owner:
		return fmt.Errorf("%w: %s owns the schema and can rewrite the audit tables", ErrWrongLogin, l.Name)
	}
	return nil
}
