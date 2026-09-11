package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// migrate brings an existing database up to the current schema.
//
// CREATE TABLE IF NOT EXISTS silently does nothing when the table already
// exists, so every column added after a cluster was first started would be
// missing -- and the failure surfaces as "no such column" at query time, long
// after startup. Additive migrations are applied here instead.
//
// Deliberately additive-only: shome never drops or rewrites a column, so a
// newer binary can run against an older database and an operator can roll back
// without losing data. Anything needing a destructive change would need a real
// versioned migration path, which this is not pretending to be.
func migrate(db *sql.DB) error {
	for _, m := range []struct {
		table  string
		column string
		decl   string
	}{
		{"jobs", "array_job", "INTEGER NOT NULL DEFAULT 0"},
		{"jobs", "array_task", "INTEGER NOT NULL DEFAULT -1"},
		{"jobs", "held", "INTEGER NOT NULL DEFAULT 0"},
		{"jobs", "dependency", "TEXT NOT NULL DEFAULT ''"},
		{"jobs", "stage_in", "INTEGER NOT NULL DEFAULT 0"},
		{"jobs", "stage_out", "TEXT NOT NULL DEFAULT '[]'"},
		{"jobs", "total_cpus", "INTEGER NOT NULL DEFAULT 0"},
		{"jobs", "total_mem", "INTEGER NOT NULL DEFAULT 0"},
		{"jobs", "total_gpumem", "INTEGER NOT NULL DEFAULT 0"},
		{"jobs", "total_gpus", "INTEGER NOT NULL DEFAULT 0"},
		{"jobs", "max_nodes", "INTEGER NOT NULL DEFAULT 0"},
		{"jobs", "fabric", "TEXT NOT NULL DEFAULT ''"},
		{"jobs", "model", "TEXT NOT NULL DEFAULT ''"},
		{"jobs", "constraint_expr", "TEXT NOT NULL DEFAULT ''"},
		{"jobs", "script_body", "BLOB"},
		{"jobs", "node_list", "TEXT NOT NULL DEFAULT '[]'"},
		{"jobs", "requeue", "INTEGER NOT NULL DEFAULT 0"},
		{"events", "hash", "TEXT NOT NULL DEFAULT ''"},
		{"nodes", "addr", "TEXT NOT NULL DEFAULT ''"},
		{"users", "qos", "TEXT NOT NULL DEFAULT ''"},
		{"jobs", "max_procs", "INTEGER NOT NULL DEFAULT 0"},
		// srun: a job somebody is watching at a terminal. Persisted rather
		// than held in memory because the agent learns from the spec it is
		// handed which session to attach to, and the spec it is handed comes
		// from here.
		{"jobs", "interactive", "INTEGER NOT NULL DEFAULT 0"},
		{"jobs", "pty", "INTEGER NOT NULL DEFAULT 0"},
		{"jobs", "stream_id", "INTEGER NOT NULL DEFAULT 0"},
		// Where in the account's storage the job runs. See job.Spec.Chdir.
		{"jobs", "chdir", "TEXT NOT NULL DEFAULT ''"},
		// How big the submitter's terminal was, so `srun --pty` starts the
		// job's terminal that size. Persisted for the same reason the rest
		// of the session's fields are: the agent learns it from the spec it
		// is handed, and that spec comes from here. Zero means the
		// submitter had no terminal to measure.
		{"jobs", "tty_rows", "INTEGER NOT NULL DEFAULT 0"},
		{"jobs", "tty_cols", "INTEGER NOT NULL DEFAULT 0"},
		{"jobs", "tty_xpixel", "INTEGER NOT NULL DEFAULT 0"},
		{"jobs", "tty_ypixel", "INTEGER NOT NULL DEFAULT 0"},
	} {
		has, err := hasColumn(db, m.table, m.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", m.table, m.column, m.decl)
		if _, err := db.Exec(stmt); err != nil {
			// A concurrent process may have added it between check and apply.
			if strings.Contains(err.Error(), "duplicate column") {
				continue
			}
			return fmt.Errorf("migrate %s.%s: %w", m.table, m.column, err)
		}
	}
	return nil
}

func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notnull    int
			dflt       sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
