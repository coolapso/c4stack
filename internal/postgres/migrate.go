package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
)

// Set is one namespace's migrations: files named NNN_name.sql at the root of Files.
// Other files, such as a README, are ignored.
type Set struct {
	Namespace string
	Files     fs.FS
}

// migration is one parsed migration file.
type migration struct {
	version  int
	name     string
	sql      string
	checksum string
}

var (
	namespacePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	filePattern      = regexp.MustCompile(`^([0-9]+)_([a-z0-9_]+)\.sql$`)
)

// lockKey identifies the advisory lock that serialises migration runs.
const lockKey = 739_084_311_982_417_601

const createTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    namespace text NOT NULL,
    version bigint NOT NULL,
    name text NOT NULL,
    checksum text NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
    applied_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, version)
)`

// load validates the sets and reads their migrations in version order.
func load(sets []Set) (map[string][]migration, error) {
	out := make(map[string][]migration, len(sets))
	for _, set := range sets {
		if !namespacePattern.MatchString(set.Namespace) {
			return nil, fmt.Errorf("postgres: invalid migration namespace %q", set.Namespace)
		}
		if _, dup := out[set.Namespace]; dup {
			return nil, fmt.Errorf("postgres: migration namespace %q is listed twice", set.Namespace)
		}
		if set.Files == nil {
			return nil, fmt.Errorf("postgres: migration namespace %q has no files", set.Namespace)
		}
		entries, err := fs.ReadDir(set.Files, ".")
		if err != nil {
			return nil, fmt.Errorf("postgres: read %s migrations: %w", set.Namespace, err)
		}
		var list []migration
		for _, e := range entries {
			if e.IsDir() || path.Ext(e.Name()) != ".sql" {
				continue
			}
			m := filePattern.FindStringSubmatch(e.Name())
			if m == nil {
				return nil, fmt.Errorf("postgres: %s migration %q is not named NNN_name.sql", set.Namespace, e.Name())
			}
			version, err := strconv.Atoi(m[1])
			if err != nil || version <= 0 {
				return nil, fmt.Errorf("postgres: %s migration %q needs a positive version", set.Namespace, e.Name())
			}
			if slices.ContainsFunc(list, func(x migration) bool { return x.version == version }) {
				return nil, fmt.Errorf("postgres: %s has two migrations with version %d", set.Namespace, version)
			}
			body, err := fs.ReadFile(set.Files, e.Name())
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(body)
			list = append(list, migration{version: version, name: m[2], sql: string(body), checksum: hex.EncodeToString(sum[:])})
		}
		slices.SortFunc(list, func(a, b migration) int { return a.version - b.version })
		out[set.Namespace] = list
	}
	return out, nil
}

// applied reads the recorded versions and checksums of one namespace.
func applied(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, namespace string) (map[int]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT version, checksum
		FROM schema_migrations
		WHERE namespace = $1`, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, err
		}
		out[version] = checksum
	}
	return out, rows.Err()
}

// plan compares a namespace's files with the database and returns the files to apply.
// newer reports versions the database has that this binary does not know, all above
// its newest file.
func plan(namespace string, files []migration, done map[int]string) (pending []migration, newer []int, err error) {
	highestKnown := 0
	if len(files) > 0 {
		highestKnown = files[len(files)-1].version
	}
	highestApplied := 0
	for version := range done {
		highestApplied = max(highestApplied, version)
	}
	for version := range done {
		if slices.ContainsFunc(files, func(m migration) bool { return m.version == version }) {
			continue
		}
		if version < highestKnown {
			return nil, nil, fmt.Errorf("postgres: %s version %d is applied but unknown to this binary", namespace, version)
		}
		newer = append(newer, version)
	}
	slices.Sort(newer)
	for _, m := range files {
		checksum, ok := done[m.version]
		switch {
		case ok && checksum != m.checksum:
			return nil, nil, fmt.Errorf("postgres: %s migration %d_%s changed after it was applied", namespace, m.version, m.name)
		case !ok && m.version < highestApplied:
			return nil, nil, fmt.Errorf("postgres: %s migration %d_%s is older than applied version %d", namespace, m.version, m.name, highestApplied)
		case !ok:
			pending = append(pending, m)
		}
	}
	return pending, newer, nil
}

// Migrate applies the pending migrations of each set, in the order given. Each file
// runs in its own transaction, and one advisory lock serialises concurrent runs.
// A database that is ahead of this binary is reported and left alone.
func Migrate(ctx context.Context, db *sql.DB, sets []Set, logger *slog.Logger) error {
	files, err := load(sets)
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, int64(lockKey)); err != nil {
		return fmt.Errorf("postgres: take the migration lock: %w", err)
	}
	defer func() {
		// Closing the connection releases the lock too; unlocking keeps the pooled
		// connection clean.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(lockKey))
	}()

	if err := adopt(ctx, conn, sets, files); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, createTable); err != nil {
		return fmt.Errorf("postgres: create schema_migrations: %w", err)
	}
	for _, set := range sets {
		done, err := applied(ctx, conn, set.Namespace)
		if err != nil {
			return err
		}
		pending, newer, err := plan(set.Namespace, files[set.Namespace], done)
		if err != nil {
			return err
		}
		if len(newer) > 0 {
			logger.Warn("the database has newer migrations than this binary; nothing to apply",
				"namespace", set.Namespace, "versions", newer)
			continue
		}
		for _, m := range pending {
			if err := apply(ctx, conn, set.Namespace, m); err != nil {
				return err
			}
			logger.Info("migration applied", "namespace", set.Namespace, "version", m.version, "name", m.name)
		}
	}
	return nil
}

func apply(ctx context.Context, conn *sql.Conn, namespace string, m migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("postgres: %s migration %d_%s: %w", namespace, m.version, m.name, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO schema_migrations (namespace, version, name, checksum)
		VALUES ($1, $2, $3, $4)`,
		namespace, m.version, m.name, m.checksum,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// legacyRegistry reports whether schema_migrations exists without a namespace column,
// as written by a runner that kept a single history.
func legacyRegistry(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (bool, error) {
	var legacy bool
	err := q.QueryRowContext(ctx, `
		SELECT to_regclass('schema_migrations') IS NOT NULL
		    AND NOT EXISTS (
		        SELECT 1
		        FROM pg_attribute
		        WHERE attrelid = to_regclass('schema_migrations')
		            AND attname = 'namespace'
		            AND NOT attisdropped
		    )`,
	).Scan(&legacy)
	return legacy, err
}

// legacyRow is one migration recorded without a namespace.
type legacyRow struct {
	version   int
	checksum  string
	namespace string
}

// legacyRows reads a registry without namespaces and assigns each row to the set that
// has a file with the same version and checksum. A row no set has, or more than one
// set has, is an error: the app must list the set that applied it.
func legacyRows(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, sets []Set, files map[string][]migration) ([]legacyRow, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT version, checksum
		FROM schema_migrations
		ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []legacyRow
	for rows.Next() {
		var r legacyRow
		if err := rows.Scan(&r.version, &r.checksum); err != nil {
			return nil, err
		}
		for _, set := range sets {
			if !slices.ContainsFunc(files[set.Namespace], func(m migration) bool {
				return m.version == r.version && m.checksum == r.checksum
			}) {
				continue
			}
			if r.namespace != "" {
				return nil, fmt.Errorf("postgres: recorded migration %d matches files in both %s and %s", r.version, r.namespace, set.Namespace)
			}
			r.namespace = set.Namespace
		}
		if r.namespace == "" {
			return nil, fmt.Errorf("postgres: recorded migration %d matches no listed migration file; list the set that applied it", r.version)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// adopt converts a registry without namespaces, so a database whose schema an app
// created with its own single migration history can join the namespaced histories.
// It runs in one transaction under the migration lock.
func adopt(ctx context.Context, conn *sql.Conn, sets []Set, files map[string][]migration) error {
	legacy, err := legacyRegistry(ctx, conn)
	if err != nil || !legacy {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := legacyRows(ctx, tx, sets, files)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE schema_migrations ADD COLUMN namespace text`); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, `
			UPDATE schema_migrations
			SET namespace = $2
			WHERE version = $1`, r.version, r.namespace,
		); err != nil {
			return err
		}
	}
	// The single history's keys, such as a unique version or name, would stop two
	// namespaces from both having a version 1.
	keys, err := tx.QueryContext(ctx, `
		SELECT conname
		FROM pg_constraint
		WHERE conrelid = 'schema_migrations'::regclass AND contype IN ('p', 'u')`)
	if err != nil {
		return err
	}
	var names []string
	for keys.Next() {
		var name string
		if err := keys.Scan(&name); err != nil {
			keys.Close()
			return err
		}
		names = append(names, name)
	}
	keys.Close()
	if err := keys.Err(); err != nil {
		return err
	}
	for _, name := range names {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE schema_migrations DROP CONSTRAINT `+quoteIdent(name)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		ALTER TABLE schema_migrations
		    ALTER COLUMN namespace SET NOT NULL,
		    ADD PRIMARY KEY (namespace, version)`); err != nil {
		return err
	}
	return tx.Commit()
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Status describes one namespace's migration state.
type Status struct {
	Namespace string
	Applied   []int
	Pending   []string // NNN_name of files not yet applied
	Newer     []int    // applied versions newer than this binary knows
}

// Report returns the migration state of each set without changing anything.
func Report(ctx context.Context, db *sql.DB, sets []Set) ([]Status, error) {
	files, err := load(sets)
	if err != nil {
		return nil, err
	}
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, err
	}
	legacy, err := legacyRegistry(ctx, db)
	if err != nil {
		return nil, err
	}
	var adopted []legacyRow
	if legacy {
		if adopted, err = legacyRows(ctx, db, sets, files); err != nil {
			return nil, err
		}
	}
	out := make([]Status, 0, len(sets))
	for _, set := range sets {
		done := map[int]string{}
		switch {
		case legacy:
			for _, r := range adopted {
				if r.namespace == set.Namespace {
					done[r.version] = r.checksum
				}
			}
		case exists:
			if done, err = applied(ctx, db, set.Namespace); err != nil {
				return nil, err
			}
		}
		pending, newer, err := plan(set.Namespace, files[set.Namespace], done)
		if err != nil {
			return nil, err
		}
		st := Status{Namespace: set.Namespace, Newer: newer}
		for version := range done {
			st.Applied = append(st.Applied, version)
		}
		slices.Sort(st.Applied)
		for _, m := range pending {
			st.Pending = append(st.Pending, fmt.Sprintf("%03d_%s", m.version, m.name))
		}
		out = append(out, st)
	}
	return out, nil
}

// Checker reports whether a database has every migration this binary needs. Newer
// migrations from a later release are tolerated, so older replicas keep serving
// during a rolling upgrade. Once satisfied, it stops querying: schemas only grow.
type Checker struct {
	sets  []Set
	files map[string][]migration
	ok    atomic.Bool
}

// NewChecker validates the sets once.
func NewChecker(sets []Set) (*Checker, error) {
	files, err := load(sets)
	if err != nil {
		return nil, err
	}
	return &Checker{sets: sets, files: files}, nil
}

// ErrSchemaNotReady means the database lacks migrations this binary needs.
var ErrSchemaNotReady = errors.New("postgres: the database schema is not ready")

// Check returns ErrSchemaNotReady until every known migration is applied.
func (c *Checker) Check(ctx context.Context, db *sql.DB) error {
	if c.ok.Load() {
		return nil
	}
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrSchemaNotReady
	}
	if legacy, err := legacyRegistry(ctx, db); err != nil || legacy {
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: the migration history has no namespaces yet", ErrSchemaNotReady)
	}
	for _, set := range c.sets {
		done, err := applied(ctx, db, set.Namespace)
		if err != nil {
			return err
		}
		pending, _, err := plan(set.Namespace, c.files[set.Namespace], done)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrSchemaNotReady, err)
		}
		if len(pending) > 0 {
			return fmt.Errorf("%w: %s has %d pending migrations", ErrSchemaNotReady, set.Namespace, len(pending))
		}
	}
	c.ok.Store(true)
	return nil
}
