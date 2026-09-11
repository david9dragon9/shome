package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/job"
)

// An older cluster's database must keep working under a newer binary.
// Without migrations this fails at query time with "no such column", which is
// both cryptic and late.
func TestOpenUpgradesAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// Build a database with an early-version jobs table: no aggregate fields,
	// no fabric, no constraint, no staging.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL DEFAULT '', user TEXT NOT NULL DEFAULT '',
  script TEXT NOT NULL DEFAULT '', args TEXT NOT NULL DEFAULT '[]',
  env TEXT NOT NULL DEFAULT '{}', workdir TEXT NOT NULL DEFAULT '',
  cpus INTEGER NOT NULL DEFAULT 1, mem_bytes INTEGER NOT NULL DEFAULT 0,
  walltime_ns INTEGER NOT NULL DEFAULT 0, gpus INTEGER NOT NULL DEFAULT 0,
  network INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '', node TEXT NOT NULL DEFAULT '',
  exit_code INTEGER NOT NULL DEFAULT 0, peak_mem INTEGER NOT NULL DEFAULT 0,
  submit_at INTEGER NOT NULL, start_at INTEGER NOT NULL DEFAULT 0,
  end_at INTEGER NOT NULL DEFAULT 0, scratch TEXT NOT NULL DEFAULT ''
);
INSERT INTO jobs (name,user,state,submit_at) VALUES ('legacy','alice','COMPLETED',1);
`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Opening with the current code must upgrade it in place.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on an older database failed: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	jobs, err := s.List(ctx, "", false)
	if err != nil {
		t.Fatalf("querying a migrated database failed: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Spec.Name != "legacy" {
		t.Fatalf("existing rows lost in migration: %+v", jobs)
	}
	// And the new columns must be usable.
	nj, err := s.Submit(ctx, job.Spec{
		Name: "new", User: "bob", Script: "/bin/true", ArrayTaskID: -1,
		Constraint: "metal", Fabric: "none", TotalCPUs: 4,
	}, time.Now())
	if err != nil {
		t.Fatalf("submitting with new fields failed: %v", err)
	}
	got, err := s.Get(ctx, nj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Constraint != "metal" || got.Spec.TotalCPUs != 4 {
		t.Errorf("new fields not persisted after migration: %+v", got.Spec)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	for i := 0; i < 3; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open #%d: %v", i+1, err)
		}
		s.Close()
	}
}

// The audit log must make tampering detectable. It cannot prevent it -- the
// database is a file on someone's disk -- but an owner lending their machine
// needs to know if the record of what ran has been rewritten.
func TestAuditChainDetectsTampering(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	for i, kind := range []string{"submitted", "started", "finished"} {
		if err := s.Event(ctx, int64(i+1), kind, "detail", now); err != nil {
			t.Fatal(err)
		}
	}
	bad, checked, err := s.VerifyAudit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if checked != 3 || bad != 0 {
		t.Fatalf("intact log reported bad=%d checked=%d", bad, checked)
	}

	// Rewrite history the way someone covering their tracks would.
	if _, err := s.db.Exec(`UPDATE events SET detail='innocent' WHERE seq=2`); err != nil {
		t.Fatal(err)
	}
	bad, _, err = s.VerifyAudit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bad != 2 {
		t.Errorf("tampering not detected: bad=%d, want 2", bad)
	}
}

func TestAuditChainSurvivesDeletion(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "b.db"))
	defer s.Close()
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		s.Event(ctx, int64(i), "kind", "d", time.Now())
	}
	// Removing a row breaks the chain at the row that followed it.
	s.db.Exec(`DELETE FROM events WHERE seq=2`)
	bad, _, err := s.VerifyAudit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bad == 0 {
		t.Error("a deleted audit row went undetected")
	}
}
