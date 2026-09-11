package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/job"
)

// A multi-node job is one job with N tasks, one per allocated node.
//
// Tracked separately from the job row because each rank succeeds or fails on
// its own machine, and the job's outcome is a function of all of them. Folding
// this into the job row would lose which node failed -- the first question
// anyone asks about a distributed run.
const taskSchema = `
CREATE TABLE IF NOT EXISTS job_tasks (
  job_id     INTEGER NOT NULL,
  rank       INTEGER NOT NULL,
  node       TEXT NOT NULL,
  state      TEXT NOT NULL,
  exit_code  INTEGER NOT NULL DEFAULT 0,
  reason     TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL DEFAULT 0,
  ended_at   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (job_id, rank)
);
CREATE INDEX IF NOT EXISTS job_tasks_node ON job_tasks(node, state);
`

// Task is one rank of a multi-node job.
type Task struct {
	JobID    int64
	Rank     int
	Node     string
	State    job.State
	ExitCode int
	Reason   string
	Started  time.Time
	Ended    time.Time
}

// CreateTasks records the allocation for a gang-scheduled job. Rank 0 is the
// coordinator: fabrics that need a rendezvous point use its node.
func (s *Store) CreateTasks(ctx context.Context, jobID int64, nodes []string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for rank, n := range nodes {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO job_tasks (job_id,rank,node,state,started_at) VALUES (?,?,?,?,?)`,
			jobID, rank, n, string(job.Running), ms(now)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) TasksFor(ctx context.Context, jobID int64) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT job_id,rank,node,state,exit_code,reason,started_at,ended_at
		 FROM job_tasks WHERE job_id=? ORDER BY rank`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var started, ended int64
		if err := rows.Scan(&t.JobID, &t.Rank, &t.Node, &t.State, &t.ExitCode, &t.Reason, &started, &ended); err != nil {
			return nil, err
		}
		t.Started, t.Ended = unms(started), unms(ended)
		out = append(out, t)
	}
	return out, rows.Err()
}

// RankOf returns the rank a node holds for a job, and whether it holds one.
func (s *Store) RankOf(ctx context.Context, jobID int64, node string) (int, bool) {
	var rank int
	err := s.db.QueryRowContext(ctx,
		`SELECT rank FROM job_tasks WHERE job_id=? AND node=?`, jobID, node).Scan(&rank)
	if err != nil {
		return 0, false
	}
	return rank, true
}

// FinishTask records one rank's outcome.
func (s *Store) FinishTask(ctx context.Context, jobID int64, node string, st job.State, code int, reason string, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE job_tasks SET state=?, exit_code=?, reason=?, ended_at=?
		 WHERE job_id=? AND node=? AND ended_at=0`,
		string(st), code, reason, ms(now), jobID, node)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no running task for job %d on node %s", jobID, node)
	}
	return nil
}

// GangOutcome reports whether every rank has finished, and the job-level
// result if so.
//
// A distributed job is only as good as its worst rank: any failure fails the
// whole job, and the reason names the node so the user knows where to look.
func (s *Store) GangOutcome(ctx context.Context, jobID int64) (done bool, st job.State, code int, reason string, err error) {
	tasks, err := s.TasksFor(ctx, jobID)
	if err != nil || len(tasks) == 0 {
		return false, "", 0, "", err
	}
	worst := job.Completed
	worstCode := 0
	var failures []string
	for _, t := range tasks {
		if !t.State.Terminal() {
			return false, "", 0, "", nil
		}
		if t.State != job.Completed {
			// Preserve the most severe outcome; OOM and TIMEOUT are more
			// informative than a generic failure.
			if worst == job.Completed || t.State == job.OOM || t.State == job.Timeout {
				worst = t.State
			}
			if worstCode == 0 {
				worstCode = t.ExitCode
			}
			d := fmt.Sprintf("rank %d on %s: %s", t.Rank, t.Node, t.State)
			if t.Reason != "" {
				d += " (" + t.Reason + ")"
			}
			failures = append(failures, d)
		}
	}
	if len(failures) > 0 {
		return true, worst, worstCode, strings.Join(failures, "; "), nil
	}
	return true, job.Completed, 0, "", nil
}

// CancelTasks marks every unfinished rank cancelled.
func (s *Store) CancelTasks(ctx context.Context, jobID int64, reason string, now time.Time) ([]string, error) {
	tasks, err := s.TasksFor(ctx, jobID)
	if err != nil {
		return nil, err
	}
	var nodes []string
	for _, t := range tasks {
		if t.State.Terminal() {
			continue
		}
		nodes = append(nodes, t.Node)
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE job_tasks SET state=?, reason=?, ended_at=? WHERE job_id=? AND ended_at=0`,
		string(job.Cancelled), reason, ms(now), jobID)
	return nodes, err
}

// RequeueTasksOnNode clears a lost node's ranks. Because a gang job needs all
// its ranks, losing one means the whole job must be requeued.
func (s *Store) RequeueTasksOnNode(ctx context.Context, node string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT job_id FROM job_tasks WHERE node=? AND ended_at=0`, node)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM job_tasks WHERE job_id=?`, id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// ClearTasks removes a job's task rows, for requeue.
func (s *Store) ClearTasks(ctx context.Context, jobID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM job_tasks WHERE job_id=?`, jobID)
	return err
}
