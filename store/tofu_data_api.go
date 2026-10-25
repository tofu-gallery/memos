package store

// Tofu (2026-10-25): this file is Tofu's addition; it is not part of upstream Memos.

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/pkg/errors"
)

// latestSchemaTableMatcher finds the tables LATEST.sql creates. Their names are plain lowercase identifiers, which
// is what makes quoting them below safe.
var latestSchemaTableMatcher = regexp.MustCompile(`(?m)^CREATE TABLE "?([a-z_][a-z0-9_]*)"? \(`)

// closeTablesToDataAPI turns on row-level security for Memos' own tables in the public schema of a Supabase
// database: one that has the roles Supabase's Data API uses, anon and authenticated. On a database whose default
// privileges still grant every new table in public to those roles, Memos' tables would otherwise be readable and
// writable through the Data API by anyone holding the project's public key. With row-level security on and no
// policy, those roles see no row, while Memos' own connection, which owns the tables, is not limited by it.
//
// It only ever turns row-level security on: a table that already has it, a table Memos' role does not own, and any
// table that is not one of Memos' own are left as they are. On any other PostgreSQL it does nothing.
func (s *Store) closeTablesToDataAPI(ctx context.Context) error {
	if s.profile == nil || s.profile.Driver != "postgres" {
		return nil
	}
	db := s.driver.GetDB()

	var dataAPIRoles bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname IN ('anon', 'authenticated'))`).Scan(&dataAPIRoles); err != nil {
		return errors.Wrap(err, "failed to look for the Data API roles")
	}
	if !dataAPIRoles {
		return nil
	}

	schema, err := migrationFS.ReadFile(s.getMigrationBasePath() + LatestSchemaFileName)
	if err != nil {
		return errors.Wrap(err, "failed to read the latest schema file")
	}
	var names []string
	for _, match := range latestSchemaTableMatcher.FindAllSubmatch(schema, -1) {
		names = append(names, string(match[1]))
	}
	if len(names) == 0 {
		return errors.New("found no table in the latest schema file")
	}

	rows, err := db.QueryContext(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
			AND c.relkind = 'r'
			AND NOT c.relrowsecurity
			AND c.relowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)
			AND c.relname = ANY($1::text[])
		ORDER BY c.relname`, "{"+strings.Join(names, ",")+"}")
	if err != nil {
		return errors.Wrap(err, "failed to list tables without row-level security")
	}
	var open []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return errors.Wrap(err, "failed to read a table name")
		}
		open = append(open, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return errors.Wrap(err, "failed to list tables without row-level security")
	}
	rows.Close()

	for _, name := range open {
		if _, err := db.ExecContext(ctx, `ALTER TABLE public."`+name+`" ENABLE ROW LEVEL SECURITY`); err != nil {
			return errors.Wrapf(err, "failed to enable row-level security on %s", name)
		}
	}
	if len(open) > 0 {
		slog.Info("enabled row-level security on Memos' tables for Supabase's Data API", slog.Int("tables", len(open)))
	}
	return nil
}
