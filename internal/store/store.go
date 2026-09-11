// Package store persists jobs and an append-only event log.
//
// SQLite in WAL mode: a home cluster's controller is a single process on a
// consumer machine that will be closed, slept and killed, so durability across
// abrupt termination matters more than throughput.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/davidwu/shome/internal/job"
	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
	// eventMu serialises audit writes so the hash chain cannot interleave.
	eventMu sync.Mutex
}

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  name        TEXT NOT NULL DEFAULT '',
  user        TEXT NOT NULL DEFAULT '',
  script      TEXT NOT NULL DEFAULT '',
  args        TEXT NOT NULL DEFAULT '[]',
  env         TEXT NOT NULL DEFAULT '{}',
  workdir     TEXT NOT NULL DEFAULT '',
  cpus        INTEGER NOT NULL DEFAULT 1,
  mem_bytes   INTEGER NOT NULL DEFAULT 0,
  walltime_ns INTEGER NOT NULL DEFAULT 0,
  gpus        INTEGER NOT NULL DEFAULT 0,
  network     INTEGER NOT NULL DEFAULT 0,
  state       TEXT NOT NULL,
  reason      TEXT NOT NULL DEFAULT '',
  node        TEXT NOT NULL DEFAULT '',
  exit_code   INTEGER NOT NULL DEFAULT 0,
  peak_mem    INTEGER NOT NULL DEFAULT 0,
  submit_at   INTEGER NOT NULL,
  start_at    INTEGER NOT NULL DEFAULT 0,
  end_at      INTEGER NOT NULL DEFAULT 0,
  scratch     TEXT NOT NULL DEFAULT '',
  array_job   INTEGER NOT NULL DEFAULT 0,
  array_task  INTEGER NOT NULL DEFAULT -1,
  held        INTEGER NOT NULL DEFAULT 0,
  dependency  TEXT NOT NULL DEFAULT '',
  stage_in    INTEGER NOT NULL DEFAULT 0,
  stage_out   TEXT NOT NULL DEFAULT '[]',
  total_cpus  INTEGER NOT NULL DEFAULT 0,
  total_mem   INTEGER NOT NULL DEFAULT 0,
  total_gpumem INTEGER NOT NULL DEFAULT 0,
  total_gpus  INTEGER NOT NULL DEFAULT 0,
  max_nodes   INTEGER NOT NULL DEFAULT 0,
  fabric      TEXT NOT NULL DEFAULT '',
  model       TEXT NOT NULL DEFAULT '',
  constraint_expr TEXT NOT NULL DEFAULT '',
  script_body BLOB,
  node_list TEXT NOT NULL DEFAULT '[]',
  requeue INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS jobs_state ON jobs(state);

-- Machines an admin removed. See ForgetNode: without this a removed machine
-- recreates its own row on the next heartbeat.
CREATE TABLE IF NOT EXISTS forgotten_nodes (
  name TEXT PRIMARY KEY,
  at   INTEGER NOT NULL
);

-- Append-only. Never updated or deleted: this is the audit trail an owner uses
-- to see what actually ran on their machine.
CREATE TABLE IF NOT EXISTS events (
  seq    INTEGER PRIMARY KEY AUTOINCREMENT,
  at     INTEGER NOT NULL,
  job_id INTEGER,
  kind   TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  -- Each row commits to the one before it: hash = H(prev_hash || fields).
  -- The table is append-only by convention, but the file is just SQLite on
  -- someone's disk -- anyone who can write it can rewrite history. Chaining
  -- does not prevent that; it makes it detectable, which is what an owner
  -- lending their machine actually needs.
  hash   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS events_job ON events(job_id);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// The scheduler is single-threaded by design; more writers would only add
	// lock contention for no benefit.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + nodeSchema + userSchema + taskSchema + serviceSchema + userKeysSchema + enrollSchema + userFilesSchema + sessionTokenSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	// Bring an existing database up to date; see migrate's comment for why
	// CREATE TABLE IF NOT EXISTS is not enough.
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// SetArrayJob links a task to its array. The array id is the first task's id,
// which is only known after that row is inserted.
func (s *Store) SetArrayJob(ctx context.Context, id, arrayJob int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET array_job=? WHERE id=?`, arrayJob, id)
	return err
}

// ArrayTasks returns every task belonging to an array.
func (s *Store) ArrayTasks(ctx context.Context, arrayJob int64) ([]*job.Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+cols+` FROM jobs WHERE array_job=? ORDER BY id ASC`, arrayJob)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*job.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func sha256sum(b []byte) []byte { h := sha256.Sum256(b); return h[:] }

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
func unms(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

// Submit records a new job in PENDING and returns it with its assigned ID.
func (s *Store) Submit(ctx context.Context, spec job.Spec, now time.Time) (*job.Job, error) {
	args, _ := json.Marshal(spec.Args)
	env, _ := json.Marshal(spec.Env)
	res, err := s.db.ExecContext(ctx, `
	  INSERT INTO jobs (name,user,script,args,env,workdir,cpus,mem_bytes,walltime_ns,gpus,network,state,submit_at,array_task,dependency,stage_in,stage_out,
	                    total_cpus,total_mem,total_gpumem,total_gpus,max_nodes,fabric,model,constraint_expr,script_body,node_list,requeue,max_procs,
	                    interactive,pty,stream_id,chdir,tty_rows,tty_cols,tty_xpixel,tty_ypixel)
	  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		spec.Name, spec.User, spec.Script, string(args), string(env), spec.Workdir,
		spec.Limits.CPUs, spec.Limits.MemBytes, int64(spec.Limits.Walltime),
		spec.Limits.GPUs, boolInt(spec.Limits.Network), string(job.Pending), ms(now), spec.ArrayTaskID,
		spec.Dependency, boolInt(spec.StageIn), string(mustJSON(spec.StageOut)),
		spec.TotalCPUs, spec.TotalMemBytes, spec.TotalGPUMem, spec.TotalGPUs, spec.MaxNodes, spec.Fabric, spec.Model, spec.Constraint, spec.ScriptBody, string(mustJSON(spec.NodeList)), boolInt(spec.Requeue), spec.Limits.MaxProcs,
		boolInt(spec.Interactive), boolInt(spec.PTY), spec.Stream, spec.Chdir,
		spec.TTYSize.Rows, spec.TTYSize.Cols, spec.TTYSize.XPixel, spec.TTYSize.YPixel)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	s.Event(ctx, id, "submitted", spec.Name, now)
	return s.Get(ctx, id)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("[]")
	}
	return b
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

const cols = `id,name,user,script,args,env,workdir,cpus,mem_bytes,walltime_ns,gpus,network,
              state,reason,node,exit_code,peak_mem,submit_at,start_at,end_at,scratch,
              array_job,array_task,held,dependency,stage_in,stage_out,
              total_cpus,total_mem,total_gpumem,total_gpus,max_nodes,fabric,model,constraint_expr,script_body,node_list,requeue,max_procs,
              interactive,pty,stream_id,chdir,tty_rows,tty_cols,tty_xpixel,tty_ypixel`

func scanJob(rs interface{ Scan(...any) error }) (*job.Job, error) {
	var j job.Job
	var args, env, stageOut string
	var body []byte
	var nodeList string
	var requeue int
	var net, stageIn int
	var interactive, pty int
	var wall int64
	var submitAt, startAt, endAt int64
	err := rs.Scan(&j.ID, &j.Spec.Name, &j.Spec.User, &j.Spec.Script, &args, &env, &j.Spec.Workdir,
		&j.Spec.Limits.CPUs, &j.Spec.Limits.MemBytes, &wall, &j.Spec.Limits.GPUs, &net,
		&j.State, &j.Reason, &j.Node, &j.ExitCode, &j.PeakMem, &submitAt, &startAt, &endAt, &j.ScratchOf,
		&j.ArrayJobID, &j.Spec.ArrayTaskID, &j.Held, &j.Spec.Dependency, &stageIn, &stageOut,
		&j.Spec.TotalCPUs, &j.Spec.TotalMemBytes, &j.Spec.TotalGPUMem, &j.Spec.TotalGPUs, &j.Spec.MaxNodes, &j.Spec.Fabric, &j.Spec.Model, &j.Spec.Constraint, &body, &nodeList, &requeue, &j.Spec.Limits.MaxProcs,
		&interactive, &pty, &j.Spec.Stream, &j.Spec.Chdir,
		&j.Spec.TTYSize.Rows, &j.Spec.TTYSize.Cols, &j.Spec.TTYSize.XPixel, &j.Spec.TTYSize.YPixel)
	if err != nil {
		return nil, err
	}
	j.Spec.ScriptBody = body
	json.Unmarshal([]byte(nodeList), &j.Spec.NodeList)
	j.Spec.Requeue = requeue != 0
	j.Spec.StageIn = stageIn != 0
	j.Spec.Interactive = interactive != 0
	j.Spec.PTY = pty != 0
	json.Unmarshal([]byte(stageOut), &j.Spec.StageOut)
	json.Unmarshal([]byte(args), &j.Spec.Args)
	json.Unmarshal([]byte(env), &j.Spec.Env)
	j.Spec.Limits.Walltime = time.Duration(wall)
	j.Spec.Limits.Network = net != 0
	j.SubmitAt, j.StartAt, j.EndAt = unms(submitAt), unms(startAt), unms(endAt)
	return &j, nil
}

func (s *Store) Get(ctx context.Context, id int64) (*job.Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM jobs WHERE id=?`, id)
	j, err := scanJob(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("job %d not found", id)
	}
	return j, err
}

// List returns jobs, newest first. user filters by owner ("" = all);
// activeOnly excludes terminal states.
func (s *Store) List(ctx context.Context, user string, activeOnly bool) ([]*job.Job, error) {
	q := `SELECT ` + cols + ` FROM jobs WHERE 1=1`
	var a []any
	if user != "" {
		q += ` AND user=?`
		a = append(a, user)
	}
	if activeOnly {
		q += ` AND state IN (?,?)`
		a = append(a, string(job.Pending), string(job.Running))
	}
	q += ` ORDER BY id DESC`
	rows, err := s.db.QueryContext(ctx, q, a...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*job.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Pending returns PENDING jobs oldest-first: FIFO order for the scheduler.
func (s *Store) Pending(ctx context.Context) ([]*job.Job, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+cols+` FROM jobs WHERE state=? AND held=0 ORDER BY id ASC`, string(job.Pending))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*job.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) MarkRunning(ctx context.Context, id int64, node, scratch string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state=?, node=?, scratch=?, start_at=?, reason='' WHERE id=?`,
		string(job.Running), node, scratch, ms(now), id)
	if err == nil {
		s.Event(ctx, id, "started", node, now)
	}
	return err
}

func (s *Store) MarkFinished(ctx context.Context, id int64, st job.State, code int, reason string, peak int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state=?, exit_code=?, reason=?, peak_mem=?, end_at=? WHERE id=?`,
		string(st), code, reason, peak, ms(now), id)
	if err == nil {
		s.Event(ctx, id, "finished", string(st)+" "+reason, now)
	}
	return err
}

// SetReason records why a job is still pending, so squeue can explain itself.
func (s *Store) SetReason(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET reason=? WHERE id=?`, reason, id)
	return err
}

// SetHeld holds or releases a pending job. Slurm keeps held jobs in PENDING
// with an explanatory reason rather than inventing a state, so we do too.
func (s *Store) SetHeld(ctx context.Context, id int64, held bool, reason string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET held=?, reason=? WHERE id=? AND state=?`,
		boolInt(held), reason, id, string(job.Pending))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("job %d is not pending", id)
	}
	return nil
}

// Requeue returns a job to PENDING, clearing its placement.
func (s *Store) Requeue(ctx context.Context, id int64, reason string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state=?, node='', start_at=0, end_at=0, exit_code=0, reason=? WHERE id=?`,
		string(job.Pending), reason, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such job %d", id)
	}
	return nil
}

func (s *Store) RecordPeak(ctx context.Context, id int64, peak int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET peak_mem=MAX(peak_mem,?) WHERE id=?`, peak, id)
	return err
}

func (s *Store) Event(ctx context.Context, jobID int64, kind, detail string, now time.Time) error {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()

	var prev string
	// A missing row (empty log) leaves prev as "", which is the defined
	// genesis value.
	s.db.QueryRowContext(ctx,
		`SELECT hash FROM events ORDER BY seq DESC LIMIT 1`).Scan(&prev)

	h := eventHash(prev, ms(now), jobID, kind, detail)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events (at,job_id,kind,detail,hash) VALUES (?,?,?,?,?)`,
		ms(now), jobID, kind, detail, h)
	return err
}

// eventHash commits a row to its predecessor.
func eventHash(prev string, at, jobID int64, kind, detail string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00%d\x00%s\x00%s", prev, at, jobID, kind, detail)
	return hex.EncodeToString(h.Sum(nil))
}

// VerifyAudit recomputes the chain and reports the first row that does not
// match, or 0 if the log is intact.
//
// Detects edited, deleted and reordered rows. It cannot detect truncation of
// the newest entries -- nothing self-contained can -- so callers who care
// should record the tip hash somewhere else.
func (s *Store) VerifyAudit(ctx context.Context) (badSeq int64, checked int, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq,at,job_id,kind,detail,hash FROM events ORDER BY seq ASC`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	prev := ""
	for rows.Next() {
		var seq, at, jobID int64
		var kind, detail, got string
		if err := rows.Scan(&seq, &at, &jobID, &kind, &detail, &got); err != nil {
			return 0, checked, err
		}
		checked++
		// Rows written before chaining existed have no hash; skip rather than
		// reporting a false tamper on an upgraded database.
		if got == "" {
			prev = ""
			continue
		}
		if want := eventHash(prev, at, jobID, kind, detail); want != got {
			return seq, checked, nil
		}
		prev = got
	}
	return 0, checked, rows.Err()
}

type Event struct {
	Seq    int64
	At     time.Time
	JobID  int64
	Kind   string
	Detail string
}

func (s *Store) Events(ctx context.Context, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq,at,job_id,kind,detail FROM events ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.Seq, &at, &e.JobID, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		e.At = unms(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// RunningOn lists the jobs recorded as running on one machine.
//
// Used to reconcile the records against what that machine says it is
// actually running: a job the node does not claim is a job nobody is
// supervising. See Controller.reclaimUnclaimed.
//
// This replaced RecoverOrphans, which failed every RUNNING job whenever the
// controller started. That could not distinguish a crash from a planned
// restart, and an agent keeps running its jobs when it loses the controller
// -- so it destroyed live work, and it overrode the deliberate choice to
// wait out an unreachable node rather than give up on its jobs.
func (s *Store) RunningOn(ctx context.Context, node string) ([]*job.Job, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+cols+` FROM jobs WHERE state=? AND node=? ORDER BY id ASC`,
		string(job.Running), node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*job.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// RequeueJob returns a job to the queue with an explanatory reason.
func (s *Store) RequeueJob(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state='PENDING', node='', start_at=0, reason=? WHERE id=?`,
		reason, id)
	return err
}

// RecentFinished returns the most recently completed jobs, newest first.
//
// A dashboard needs these alongside the queue: an empty queue means either
// "nothing to do" or "everything just failed", and only the recent history
// distinguishes them.
func (s *Store) RecentFinished(ctx context.Context, limit int) ([]*job.Job, error) {
	if limit <= 0 {
		limit = 15
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+cols+` FROM jobs WHERE state NOT IN (?,?) ORDER BY id DESC LIMIT ?`,
		string(job.Pending), string(job.Running), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*job.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
