package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/job"
)

// A service is a job shome keeps running.
//
// Batch jobs alone cannot support anything built on top of shome: reloading a
// 130 GB model for every request is not viable, so something has to hold an
// allocation across requests. A service is that -- but it stays a scheduled
// citizen, subject to the same owner policy and preemption as any other job,
// rather than a privileged process outside the scheduler.
const serviceSchema = `
CREATE TABLE IF NOT EXISTS services (
  name         TEXT PRIMARY KEY,
  owner        TEXT NOT NULL,
  spec         TEXT NOT NULL,
  desired      TEXT NOT NULL DEFAULT 'running',
  job_id       INTEGER NOT NULL DEFAULT 0,
  endpoint     TEXT NOT NULL DEFAULT '',
  restarts     INTEGER NOT NULL DEFAULT 0,
  last_start   INTEGER NOT NULL DEFAULT 0,
  idle_timeout INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL
);
`

// Service is a long-lived allocation.
type Service struct {
	Name        string
	Owner       string
	Spec        job.Spec
	Desired     string // "running" | "stopped"
	JobID       int64
	Endpoint    string
	Restarts    int
	LastStart   time.Time
	IdleTimeout time.Duration
	CreatedAt   time.Time
}

func (s *Store) CreateService(ctx context.Context, svc Service, now time.Time) error {
	b, err := json.Marshal(svc.Spec)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
	  INSERT INTO services (name,owner,spec,desired,idle_timeout,created_at)
	  VALUES (?,?,?,?,?,?)`,
		svc.Name, svc.Owner, string(b), "running", int64(svc.IdleTimeout), ms(now))
	if err != nil && isUnique(err) {
		return fmt.Errorf("service %q already exists", svc.Name)
	}
	return err
}

func isUnique(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "UNIQUE") || strings.Contains(m, "constraint failed")
}

func scanService(rs interface{ Scan(...any) error }) (*Service, error) {
	var svc Service
	var spec string
	var idle, last, created int64
	if err := rs.Scan(&svc.Name, &svc.Owner, &spec, &svc.Desired, &svc.JobID,
		&svc.Endpoint, &svc.Restarts, &last, &idle, &created); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(spec), &svc.Spec)
	svc.IdleTimeout = time.Duration(idle)
	svc.LastStart, svc.CreatedAt = unms(last), unms(created)
	return &svc, nil
}

const svcCols = `name,owner,spec,desired,job_id,endpoint,restarts,last_start,idle_timeout,created_at`

func (s *Store) Services(ctx context.Context, owner string) ([]*Service, error) {
	q := `SELECT ` + svcCols + ` FROM services`
	var a []any
	if owner != "" {
		q += ` WHERE owner=?`
		a = append(a, owner)
	}
	q += ` ORDER BY name`
	rows, err := s.db.QueryContext(ctx, q, a...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Service
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

func (s *Store) Service(ctx context.Context, name string) (*Service, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+svcCols+` FROM services WHERE name=?`, name)
	svc, err := scanService(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no such service %q", name)
	}
	return svc, err
}

func (s *Store) SetServiceJob(ctx context.Context, name string, jobID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE services SET job_id=?, last_start=?, restarts=restarts+1 WHERE name=?`,
		jobID, ms(now), name)
	return err
}

func (s *Store) SetServiceDesired(ctx context.Context, name, desired string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE services SET desired=? WHERE name=?`, desired, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such service %q", name)
	}
	return nil
}

func (s *Store) SetServiceEndpoint(ctx context.Context, name, endpoint string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE services SET endpoint=? WHERE name=?`, endpoint, name)
	return err
}

func (s *Store) DeleteService(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM services WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such service %q", name)
	}
	return nil
}
