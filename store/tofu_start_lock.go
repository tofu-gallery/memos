package store

// Tofu (2026-10-26): this file is Tofu's addition; it is not part of upstream Memos.
//
// Several starts of one copy can run at the same moment: Tofu's hosting may start the app more than once within a
// second or two when a new deployment gets its first requests, and those starts share nothing but the database. On
// an empty database each of them would run LATEST.sql at once, and all but one would stop with `duplicate key value
// violates unique constraint "pg_type_typname_nsp_index"` or `relation ... already exists`. So on PostgreSQL every
// step that changes the schema runs in one transaction whose first statement takes one advisory lock,
// pg_advisory_xact_lock(tofuSchemaLockKey), and reads the state again once it holds it. One start does the work while
// the others wait, and then they find it done.
//
// A transaction-level lock is what holds through a transaction pooler, which is what Tofu's DATABASE_URL points at
// (port 6543): the pooler keeps one server connection with one client from BEGIN to COMMIT and only lends it to
// other clients between transactions, and PostgreSQL releases the lock when that transaction ends, however it ends.
// A session-level lock (pg_advisory_lock) would stay on a server connection that the pooler goes on lending. Because
// the work runs in the same transaction as the lock, a start never needs a second server connection while it holds
// the lock, and a start that stops halfway leaves nothing behind: its transaction rolls back.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"uuid"

	"github.com/pkg/errors"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/usememos/memos/internal/version"
	storepb "github.com/usememos/memos/proto/gen/store"
)

const (
	// tofuSchemaLockKey is the advisory lock key every start of Memos takes for its schema step. Any fixed number
	// does, as long as every start uses the same one.
	tofuSchemaLockKey int64 = 7317265330421026
	// tofuSchemaLockWait bounds how long a start waits for another start's schema step. A start that waits longer
	// stops with an error instead of changing the schema beside it, and the host starts it again.
	tofuSchemaLockWait = "120s"
)

// tofuLockSchema makes tx the transaction that holds the schema lock, waiting at most tofuSchemaLockWait for it. The
// wait bound applies to the lock alone: the statements after it wait for table locks as upstream's do. One round trip.
func tofuLockSchema(ctx context.Context, tx *sql.Tx) error {
	stmt := fmt.Sprintf("SET LOCAL lock_timeout = '%s'; SELECT pg_advisory_xact_lock(%d); SET LOCAL lock_timeout TO DEFAULT", tofuSchemaLockWait, tofuSchemaLockKey)
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if tofuSQLState(err) == "55P03" {
			return errors.Errorf("another start of Memos has held the schema lock for over %s; stopping instead of changing the schema beside it", tofuSchemaLockWait)
		}
		return errors.Wrap(err, "failed to take the schema lock")
	}
	return nil
}

// tofuSQLState returns the SQLSTATE of the PostgreSQL error in err's chain, or "" when there is none.
func tofuSQLState(err error) string {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState()
	}
	return ""
}

// tofuDuplicateObject reports whether err says that something the schema step makes is there already, which is what
// a start meets when something that did not take the lock made it beside this one.
func tofuDuplicateObject(err error) bool {
	switch tofuSQLState(err) {
	case "42P07", // duplicate_table: relation ... already exists
		"42710", // duplicate_object
		"42P06", // duplicate_schema
		"42723", // duplicate_function
		"42701", // duplicate_column
		"23505": // unique_violation, such as pg_type_typname_nsp_index when two CREATE TABLEs cross
		return true
	}
	return false
}

// tofuInitializeSchema is preMigrate's work on an empty PostgreSQL database (see the top of this file). A start that
// still meets an object made beside it tries the whole step once more.
func (s *Store) tofuInitializeSchema(ctx context.Context) error {
	err := s.tofuInitializeSchemaOnce(ctx)
	if err != nil && tofuDuplicateObject(err) {
		slog.Warn("the schema step met an object made beside it; trying the step once more", slog.String("error", err.Error()))
		err = s.tofuInitializeSchemaOnce(ctx)
	}
	return err
}

// tofuInitializeSchemaOnce runs, in one transaction that holds the schema lock: LATEST.sql, the schema version, the
// key that signs sign-in tokens, and row-level security for Supabase's Data API (tofu_data_api.go). Upstream writes
// the version after its transaction, and server.go makes the key later in every start that finds none: a start that
// came in between would stop because the database looks too old to upgrade, or keep a key of its own and refuse the
// sign-ins the other starts hand out. Here a start that waited for the lock finds all of it, or none of it.
func (s *Store) tofuInitializeSchemaOnce(ctx context.Context) error {
	tx, err := s.driver.GetDB().BeginTx(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "failed to start transaction")
	}
	defer tx.Rollback()
	if err := tofuLockSchema(ctx, tx); err != nil {
		return err
	}

	// Read the state again inside the lock: another start may have made the schema while this one waited.
	var initialized bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_catalog = current_database() AND table_name = 'memo' AND table_type = 'BASE TABLE')").Scan(&initialized); err != nil {
		return errors.Wrap(err, "failed to check if database is initialized")
	}
	if initialized {
		slog.Info("schema already in place: another start made it while this one waited")
		return tx.Commit()
	}

	filePath := s.getMigrationBasePath() + LatestSchemaFileName
	bytes, err := migrationFS.ReadFile(filePath)
	if err != nil {
		return errors.Wrapf(err, "failed to read latest schema file %s", filePath)
	}
	slog.Info("initializing new database with latest schema", slog.String("file", filePath))
	if _, err := tx.ExecContext(ctx, string(bytes)); err != nil {
		return errors.Wrapf(err, "failed to execute SQL file %s", filePath)
	}
	schemaVersion, err := s.GetCurrentSchemaVersion()
	if err != nil {
		return errors.Wrap(err, "failed to get current schema version")
	}
	if err := tofuSaveBasicSetting(ctx, tx, &storepb.InstanceBasicSetting{SchemaVersion: schemaVersion, SecretKey: uuid.NewV4().String()}); err != nil {
		return err
	}
	if err := s.closeTablesToDataAPIInTx(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return errors.Wrap(err, "failed to commit transaction")
	}
	slog.Info("database initialized successfully", slog.String("schemaVersion", schemaVersion))
	return nil
}

// tofuStoredBasicSetting reads the stored BASIC setting (schema version and signing key) inside tx, bypassing the
// store's cache, which may hold what this start read before it waited for the lock.
func tofuStoredBasicSetting(ctx context.Context, tx *sql.Tx) (*storepb.InstanceBasicSetting, error) {
	basic := &storepb.InstanceBasicSetting{}
	var value string
	err := tx.QueryRowContext(ctx, "SELECT value FROM system_setting WHERE name = $1", storepb.InstanceSettingKey_BASIC.String()).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return basic, nil
	}
	if err != nil {
		return nil, errors.Wrap(err, "failed to read the instance basic setting")
	}
	if err := protojsonUnmarshaler.Unmarshal([]byte(value), basic); err != nil {
		return nil, errors.Wrap(err, "failed to read the instance basic setting")
	}
	return basic, nil
}

// tofuSaveBasicSetting writes the BASIC setting inside tx, as UpsertInstanceSetting would.
func tofuSaveBasicSetting(ctx context.Context, tx *sql.Tx, basic *storepb.InstanceBasicSetting) error {
	value, err := protojson.Marshal(basic)
	if err != nil {
		return errors.Wrap(err, "failed to marshal instance basic setting")
	}
	stmt := "INSERT INTO system_setting (name, value, description) VALUES ($1, $2, '') ON CONFLICT (name) DO UPDATE SET value = EXCLUDED.value"
	if _, err := tx.ExecContext(ctx, stmt, storepb.InstanceSettingKey_BASIC.String(), string(value)); err != nil {
		return errors.Wrap(err, "failed to save the instance basic setting")
	}
	return nil
}

// tofuLockMigrations is the start of applyMigrations' transaction on PostgreSQL: it takes the schema lock, then reads
// the stored schema version again. It returns that version, and done when another start applied the migrations up
// to targetSchemaVersion while this one waited.
func (s *Store) tofuLockMigrations(ctx context.Context, tx *sql.Tx, targetSchemaVersion string) (string, bool, error) {
	if err := tofuLockSchema(ctx, tx); err != nil {
		return "", false, err
	}
	basic, err := tofuStoredBasicSetting(ctx, tx)
	if err != nil {
		return "", false, err
	}
	if !isVersionEmpty(basic.SchemaVersion) && !version.IsVersionGreaterThan(targetSchemaVersion, basic.SchemaVersion) {
		slog.Info("migrations already applied: another start applied them while this one waited", slog.String("schemaVersion", basic.SchemaVersion))
		s.instanceSettingCache.Delete(ctx, storepb.InstanceSettingKey_BASIC.String())
		return basic.SchemaVersion, true, nil
	}
	return basic.SchemaVersion, false, nil
}

// tofuFinishMigrations is the end of applyMigrations' transaction on PostgreSQL: the new schema version, saved in the
// same transaction so that a start that waited finds the migrations and the version together, and row-level security
// for any table the migrations made.
func (s *Store) tofuFinishMigrations(ctx context.Context, tx *sql.Tx, targetSchemaVersion string) error {
	basic, err := tofuStoredBasicSetting(ctx, tx)
	if err != nil {
		return err
	}
	basic.SchemaVersion = targetSchemaVersion
	if err := tofuSaveBasicSetting(ctx, tx, basic); err != nil {
		return err
	}
	return s.closeTablesToDataAPIInTx(ctx, tx)
}
