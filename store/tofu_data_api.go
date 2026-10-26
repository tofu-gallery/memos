package store

// Tofu (2026-10-25): this file is Tofu's addition; it is not part of upstream Memos.
// Tofu (2026-10-26): it now runs inside a transaction that holds the schema lock (tofu_start_lock.go), sends its
// statements in two round trips, and covers every table Memos' role owns in its schema, not only LATEST.sql's.

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"

	"github.com/pkg/errors"
)

// closeTablesToDataAPI turns on row-level security for Memos' tables on a Supabase database (see
// closeTablesToDataAPIInTx), in a transaction of its own that holds the schema lock, so that it never runs beside
// another start's schema step. On an empty database the schema step has done it already; this covers a database
// whose tables were made without it. On any other PostgreSQL, and on SQLite and MySQL, it does nothing.
func (s *Store) closeTablesToDataAPI(ctx context.Context) error {
	if s.profile == nil || s.profile.Driver != "postgres" {
		return nil
	}
	tx, err := s.driver.GetDB().BeginTx(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "failed to start transaction")
	}
	defer tx.Rollback()
	if err := tofuLockSchema(ctx, tx); err != nil {
		return err
	}
	if err := s.closeTablesToDataAPIInTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// closeTablesToDataAPIInTx turns on row-level security, inside tx, for every ordinary table that Memos' role owns in
// the schema Memos makes its tables in (current_schema(), public on Tofu), when the database has the roles Supabase's
// Data API uses, anon and authenticated. On a database whose default privileges still grant every new table in
// public to those roles, Memos' tables would otherwise be readable and writable through the Data API by anyone
// holding the project's public key, `user` with its password hashes included. With row-level security on and no
// policy, those roles see no row and can write none, while Memos' own connection, which owns the tables, is not
// limited by it. It only ever turns row-level security on, and leaves tables Memos' role does not own as they are.
func (s *Store) closeTablesToDataAPIInTx(ctx context.Context, tx *sql.Tx) error {
	if s.profile == nil || s.profile.Driver != "postgres" {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT format('ALTER TABLE %I.%I ENABLE ROW LEVEL SECURITY', n.nspname, c.relname)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema()
			AND c.relkind IN ('r', 'p')
			AND NOT c.relrowsecurity
			AND c.relowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)
			AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname IN ('anon', 'authenticated'))
		ORDER BY c.relname`)
	if err != nil {
		return errors.Wrap(err, "failed to list tables without row-level security")
	}
	var statements []string
	for rows.Next() {
		var statement string
		if err := rows.Scan(&statement); err != nil {
			rows.Close()
			return errors.Wrap(err, "failed to read a table name")
		}
		statements = append(statements, statement)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return errors.Wrap(err, "failed to list tables without row-level security")
	}
	rows.Close()
	if len(statements) == 0 {
		return nil
	}

	// One round trip: format's %I quotes each name, and the statements go in one simple query.
	if _, err := tx.ExecContext(ctx, strings.Join(statements, ";\n")); err != nil {
		return errors.Wrap(err, "failed to enable row-level security")
	}
	slog.Info("enabled row-level security on Memos' tables for Supabase's Data API", slog.Int("tables", len(statements)))
	return nil
}
