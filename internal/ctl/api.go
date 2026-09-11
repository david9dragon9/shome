package ctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/fabric"
	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/sched"
	"github.com/davidwu/shome/internal/sshca"
	"github.com/davidwu/shome/internal/store"
)

// API serves the control plane: the same handlers for every way in.
//
// Reached three ways -- a unix socket for clients on the controller itself,
// the web console, and forwarded from a node over its mTLS connection -- and
// all three mount this one mux. Nothing gets a privileged path of its own, so
// anything clickable or forwardable is also scriptable, and there is one place
// where authentication happens.
//
// The socket's file permissions authenticate the machine account, not the
// shome user, so they are not the access control: every route carries a
// bearer credential regardless of how it arrived.
type API struct {
	c *Controller
	// routeTable records what routes() registered. Written once per routes()
	// call and read only for reporting, which is what lets a test assert over
	// the whole route table instead of a sample of it.
	routeTable []routeInfo
}

type JobView struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	User     string `json:"user"`
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	Node     string `json:"node,omitempty"`
	ExitCode int    `json:"exit_code"`
	PeakMiB  int64  `json:"peak_mib"`
	CPUs     int    `json:"cpus"`
	MemMiB   int64  `json:"mem_mib"`
	GPUs     int    `json:"gpus"`
	Elapsed  string `json:"elapsed"`
	TimeLim  string `json:"time_limit"`
	Scratch  string `json:"scratch,omitempty"`
	SubmitAt string `json:"submit_at"`

	// Live usage, filled in by the dashboard. LiveMiB is -1 when the job's
	// node has not reported, which is different from a job using no memory.
	LiveMiB   int64 `json:"live_mib"`
	NProcs    int   `json:"nprocs,omitempty"`
	Throttled bool  `json:"throttled,omitempty"`

	Held       bool   `json:"held,omitempty"`
	Dependency string `json:"dependency,omitempty"`
	Label      string `json:"label"`                 // "12" or "12_3" for array tasks
	ArrayTasks int    `json:"array_tasks,omitempty"` // set on the submit response only

	// Stream is the interactive session to attach to, for srun. Zero for an
	// ordinary batch job.
	Stream int64 `json:"stream,omitempty"`

	// Note is advice about how this job will run that the submitter would
	// otherwise discover the hard way.
	Note string `json:"note,omitempty"`

	// Priority is the score the scheduler used to place this job in the
	// queue, and Scored says whether it has one -- a job that is already
	// running, or a cluster with priority off, has none. Distinguishing
	// those from a genuine zero is why there are two fields.
	Priority float64 `json:"priority,omitempty"`
	Scored   bool    `json:"scored,omitempty"`
}

func view(j *job.Job, now time.Time) JobView {
	tl := "UNLIMITED"
	if j.Spec.Limits.Walltime > 0 {
		tl = job.FormatDuration(j.Spec.Limits.Walltime)
	}
	return JobView{
		Label: j.Label(),
		ID:    j.ID, Name: j.Spec.Name, User: j.Spec.User, State: string(j.State),
		Reason: j.Reason, Node: j.Node, ExitCode: j.ExitCode, PeakMiB: j.PeakMem >> 20,
		CPUs: j.Spec.Limits.CPUs, MemMiB: j.Spec.Limits.MemBytes >> 20, GPUs: j.Spec.Limits.GPUs,
		Held: j.Held, Dependency: j.Spec.Dependency,
		Elapsed: job.FormatDuration(j.Elapsed(now)), TimeLim: tl,
		Scratch: j.ScratchOf, SubmitAt: j.SubmitAt.Format(time.RFC3339),
	}
}

// submitNote warns about how a job will run, where that differs from what
// the submitter is likely to expect.
//
// A GPU job on a Mac is the case that matters: Metal does not exist inside a
// virtual machine, so it runs natively, without the filesystem isolation
// every other job gets. It still has the base software -- shome opens the
// sandbox onto the machine's own copies -- but the machine is visible to it,
// and that difference is worth saying out loud rather than leaving to be
// discovered.
func (a *API) submitNote(ctx context.Context, spec job.Spec) string {
	if spec.Limits.GPUs <= 0 {
		return ""
	}
	nodes, err := a.c.Store().Nodes(ctx)
	if err != nil {
		return ""
	}
	var macs []string
	for _, n := range nodes {
		if n.Caps.OS == "darwin" && n.Caps.GPUs > 0 {
			macs = append(macs, a.c.LabelOf(n.Name))
		}
	}
	if len(macs) == 0 {
		return ""
	}
	sort.Strings(macs)
	return fmt.Sprintf("this job asks for a GPU, so on a Mac (%s) it runs "+
		"natively rather than in a container: it has the usual software, but "+
		"the machine's own filesystem is visible to it and paths are the "+
		"host's. Drop --gres to get the contained environment.",
		strings.Join(macs, ", "))
}

// namedJob shows the machine a job ran on by the name the admin gave it, and
// attaches the priority the scheduler gave the job.
func (a *API) namedJob(v JobView) JobView {
	v.Node = a.c.LabelOf(v.Node)
	// Only while it is queued. A priority is a queue position, so reporting
	// one for a job that is already running -- which happens for a pass or
	// two after it starts -- describes a decision that has been made and
	// invites the reader to think it still matters.
	if v.State == string(job.Pending) {
		if s, ok := a.c.ScoreOf(v.ID); ok {
			v.Priority, v.Scored = s.Priority, true
		}
	}
	return v
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// authedHandler is a handler that has already been given the caller's account.
type authedHandler func(http.ResponseWriter, *http.Request, *store.User)

// routeInfo records one registered route and the role it demands, so the
// invariant in routes() -- that no route is reachable without a credential --
// can be asserted by a test over the whole table rather than a chosen few.
type routeInfo struct {
	Pattern string
	MinRole store.Role
}

// registeredRoutes reports every route routes() installed, with its guard.
func (a *API) registeredRoutes() []routeInfo { return a.routeTable }

// roleAny admits any authenticated account. RoleUser is the lowest rank, so
// requiring it is exactly "present a working credential"; it is named rather
// than spelled store.RoleUser at each call site because "any account" and
// "specifically the user role" are different intentions.
const roleAny = store.RoleUser

func (a *API) routes() *http.ServeMux {
	m := http.NewServeMux()
	a.routeTable = nil
	// Every route is authenticated: reg is the only way one is registered, and
	// it takes the required role as an argument rather than accepting a
	// pre-wrapped handler, so there is no spelling of a route that forgets the
	// guard. Ownership is checked per job inside the handlers; anything
	// cluster-wide requires an admin account.
	reg := func(pattern string, min store.Role, h authedHandler) {
		a.routeTable = append(a.routeTable, routeInfo{Pattern: pattern, MinRole: min})
		m.HandleFunc(pattern, a.requireRole(min, h))
	}
	reg("POST /submit", roleAny, a.submit)
	reg("GET /jobs", roleAny, a.jobs)
	reg("GET /job/{id}", roleAny, a.getJob)
	reg("POST /cancel/{id}", roleAny, a.cancel)
	// Both spellings, one handler: /node is what the CLI has always called
	// and /nodes is what the console uses.
	reg("GET /node", roleAny, a.nodesInfo)
	reg("GET /nodes", roleAny, a.nodesInfo)
	reg("GET /events", roleAny, a.events)
	reg("GET /whoami", roleAny, a.whoami)
	reg("GET /job/{id}/output", roleAny, a.output)
	reg("POST /job/{id}/stagein", roleAny, a.uploadStageIn)
	reg("GET /job/{id}/stageout", roleAny, a.downloadStageOut)
	reg("POST /job/{id}/hold", roleAny, a.hold)
	reg("POST /job/{id}/release", roleAny, a.release)
	reg("POST /job/{id}/requeue", roleAny, a.requeue)

	reg("POST /shutdown", store.RoleAdmin, a.shutdown)
	reg("POST /node/{name}/drain", store.RoleOperator, a.drain)
	reg("POST /node/{name}/resume", store.RoleOperator, a.resume)
	reg("POST /node/{name}/stop", store.RoleAdmin, a.stopNode)
	reg("POST /node/{name}/remove", store.RoleAdmin, a.removeNode)
	reg("POST /token", store.RoleAdmin, a.token)
	reg("GET /storage", roleAny, a.storageList)
	reg("POST /storage", roleAny, a.storagePut)
	reg("GET /storage/file", roleAny, a.storageGet)
	reg("DELETE /storage", roleAny, a.storageDel)
	reg("POST /services", roleAny, a.createService)
	reg("GET /services", roleAny, a.listServices)
	reg("DELETE /services/{name}", roleAny, a.deleteService)
	reg("POST /services/{name}/stop", roleAny, a.stopService)
	reg("POST /services/{name}/start", roleAny, a.startService)
	reg("POST /sshcert", roleAny, a.sshCert)
	reg("POST /plan", roleAny, a.plan)

	reg("POST /emergency/drain-all", store.RoleOperator, a.drainAll)
	reg("POST /users/{name}/quarantine", store.RoleAdmin, a.quarantine)
	reg("POST /users/{name}/unquarantine", store.RoleAdmin, a.unquarantine)
	reg("GET /audit", store.RoleOperator, a.audit)
	reg("GET /audit/verify", store.RoleOperator, a.auditVerify)

	// One request returning the whole live view. See dashboard.go for why the
	// CLI and web console share it rather than each assembling their own.
	reg("GET /dashboard", roleAny, a.dashboard)
	reg("GET /node/{name}/history", roleAny, a.nodeHistory)

	// Who may log in to the login node.
	reg("POST /keys", store.RoleAdmin, a.addUserKey)
	reg("GET /keys", roleAny, a.listUserKeys)
	reg("DELETE /keys/{fingerprint...}", store.RoleAdmin, a.delUserKey)
	reg("POST /users/{name}/enroll", store.RoleAdmin, a.enrollCode)
	reg("POST /enroll", roleAny, a.enrollSelf)
	reg("POST /unenroll", roleAny, a.unenroll)
	reg("POST /users/{name}/unenroll", store.RoleAdmin, a.adminUnenroll)
	reg("DELETE /codes/{id}", store.RoleAdmin, a.invalidateCode)

	// Per-user files, which exist per machine because there is no shared
	// filesystem to put them in one place.
	reg("GET /fs", roleAny, a.fsList)
	// Interactive sessions: srun's live output, and srun --pty.
	reg("GET /stream/{id}/out", roleAny, a.streamOut)
	reg("POST /stream/{id}/in", roleAny, a.streamIn)
	reg("POST /stream/{id}/resize", roleAny, a.streamResize)
	reg("GET /stream/{id}", roleAny, a.streamStatus)
	reg("GET /quota", roleAny, a.quotaFor)
	reg("GET /login", roleAny, a.loginWhere)
	reg("GET /cluster", roleAny, a.clusterInfo)
	reg("POST /cluster", store.RoleAdmin, a.setClusterName)
	reg("POST /cluster/node", store.RoleAdmin, a.setNodeName)
	reg("GET /fs/usage", roleAny, a.fsUsage)
	reg("POST /fs/remove", roleAny, a.fsRemove)
	reg("POST /fs/transfer", roleAny, a.fsTransfer)
	reg("GET /fs/transfer/{id}", roleAny, a.fsTransferStatus)
	reg("POST /fs/upload", roleAny, a.fsUpload)
	reg("POST /fs/fetch", roleAny, a.fsFetch)
	reg("GET /fs/fetch/{id}", roleAny, a.fsFetchBody)

	// Priority and fair-share. Readable by anyone -- a user has to be able
	// to see why their job is behind somebody else's -- writable by admins.
	reg("GET /priority", roleAny, a.getPriority)
	reg("POST /priority", store.RoleAdmin, a.setPriority)
	reg("GET /share", roleAny, a.shareInfo)
	reg("POST /users/{name}/shares", store.RoleAdmin, a.setUserShares)
	reg("GET /job/{id}/priority", roleAny, a.jobPriority)
	reg("GET /qos", roleAny, a.getQoS)
	reg("POST /qos", store.RoleAdmin, a.setQoSDefaults)
	reg("POST /qos/cluster", store.RoleAdmin, a.setQoSCluster)
	reg("POST /users/{name}/qos", store.RoleAdmin, a.setUserQoS)

	reg("GET /users", store.RoleAdmin, a.listUsers)
	reg("POST /users", store.RoleAdmin, a.addUser)
	reg("DELETE /users/{name}", store.RoleAdmin, a.delUser)
	return m
}

// whoami lets the CLI show which identity a token maps to.
func (a *API) whoami(w http.ResponseWriter, r *http.Request, u *store.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"user": u.Name, "role": string(u.Role), "quota_mib": u.QuotaBytes >> 20,
	})
}

// drainAll is the break-glass lever: stop the whole cluster taking new work.
//
// Drains rather than kills. An operator reaching for this is usually reacting
// to something going wrong, and destroying running work would make a bad
// situation worse and irreversible.
func (a *API) drainAll(w http.ResponseWriter, r *http.Request, u *store.User) {
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "emergency drain by " + u.Name
	}
	nodes, err := a.c.Store().Nodes(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	var drained []string
	for _, n := range nodes {
		if n.State == store.NodeDown {
			continue
		}
		if err := a.c.DrainNode(r.Context(), n.Name, reason); err == nil {
			drained = append(drained, n.Name)
		}
	}
	a.c.Store().Event(r.Context(), 0, "emergency_drain_all",
		fmt.Sprintf("%s: %s (%d nodes)", u.Name, reason, len(drained)), time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"drained": drained, "reason": reason})
}

// quarantine freezes an account: its running jobs are cancelled and its token
// stops working, in one action.
func (a *API) quarantine(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	if name == u.Name {
		fail(w, http.StatusBadRequest, "refusing to quarantine the account you are using")
		return
	}
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "quarantined by " + u.Name
	}
	if err := a.c.Store().SetUserDisabled(r.Context(), name, true); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// Drop any session they have open. Quarantine that only stops the next
	// login leaves the person it was aimed at still connected.
	//
	// Their session credentials go too. The API rejects a disabled account
	// on every request, so this is belt and braces rather than the only
	// barrier -- but leaving live credentials for a quarantined account in
	// the database is not a state worth having.
	if err := a.c.Store().DeleteSessionTokensFor(r.Context(), name); err != nil {
		a.c.log.Warn("could not revoke session credentials", "user", name, "err", err)
	}
	if a.c.onRevoke != nil {
		a.c.onRevoke(name, "")
	}
	jobs, err := a.c.Store().List(r.Context(), name, true)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	var cancelled []int64
	for _, j := range jobs {
		if err := a.c.Cancel(r.Context(), j.ID); err == nil {
			cancelled = append(cancelled, j.ID)
		}
	}
	a.c.Store().Event(r.Context(), 0, "quarantine", name+": "+reason, time.Now())
	writeJSON(w, http.StatusOK, map[string]any{
		"user": name, "disabled": true, "cancelled_jobs": cancelled, "reason": reason,
	})
}

func (a *API) unquarantine(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	if err := a.c.Store().SetUserDisabled(r.Context(), name, false); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.c.Store().Event(r.Context(), 0, "unquarantine", name+" by "+u.Name, time.Now())
	writeJSON(w, http.StatusOK, map[string]string{"user": name, "status": "restored"})
}

// audit returns the cluster-wide event log.
func (a *API) audit(w http.ResponseWriter, r *http.Request, _ *store.User) {
	n := 200
	if v := r.URL.Query().Get("n"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			n = p
		}
	}
	evs, err := a.c.Store().Events(r.Context(), n)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, evs)
}

// auditVerify recomputes the audit hash chain.
func (a *API) auditVerify(w http.ResponseWriter, r *http.Request, _ *store.User) {
	bad, checked, err := a.c.Store().VerifyAudit(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"intact": bad == 0, "checked": checked, "first_bad_seq": bad,
	})
}

func (a *API) listUsers(w http.ResponseWriter, r *http.Request, _ *store.User) {
	us, err := a.c.Store().Users(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	type uv struct {
		Name     string `json:"name"`
		Role     string `json:"role"`
		QuotaMiB int64  `json:"quota_mib"`
		Disabled bool   `json:"disabled"`
	}
	out := make([]uv, 0, len(us))
	for _, u := range us {
		out = append(out, uv{u.Name, string(u.Role), u.QuotaBytes >> 20, u.Disabled})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) addUser(w http.ResponseWriter, r *http.Request, _ *store.User) {
	var req struct {
		Name  string `json:"name"`
		Role  string `json:"role"`
		Quota string `json:"quota"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad request body")
		return
	}
	role := store.RoleUser
	if req.Role != "" {
		r, ok := store.ValidRole(req.Role)
		if !ok {
			fail(w, http.StatusBadRequest,
				"unknown role "+req.Role+" (known: admin, operator, user)")
			return
		}
		role = r
	}
	var quota int64
	if req.Quota != "" {
		q, err := ParseMem(req.Quota)
		if err != nil {
			fail(w, http.StatusBadRequest, "bad quota: "+err.Error())
			return
		}
		quota = q
	}
	tok, err := a.c.Store().CreateUser(r.Context(), req.Name, role, quota, time.Now())
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.c.Store().Event(r.Context(), 0, "user_add", req.Name, time.Now())
	writeJSON(w, http.StatusOK, map[string]string{"name": req.Name, "token": tok})
}

func (a *API) delUser(w http.ResponseWriter, r *http.Request, caller *store.User) {
	name := r.PathValue("name")
	if name == caller.Name {
		fail(w, http.StatusBadRequest, "refusing to delete the account you are using")
		return
	}
	if err := a.c.Store().DeleteUser(r.Context(), name); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.c.Store().Event(r.Context(), 0, "user_delete", name, time.Now())
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// SubmitRequest is a job plus an optional array specification.
type SubmitRequest struct {
	job.Spec
	Array string `json:"array,omitempty"`
	// Hold submits the job held, so the client can upload inputs before it
	// becomes a scheduling candidate.
	Hold bool `json:"hold,omitempty"`
}

func (a *API) submit(w http.ResponseWriter, r *http.Request, caller *store.User) {
	var req SubmitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	spec := req.Spec
	// Identity is taken from the authenticated token, never from the body:
	// otherwise any caller could submit work billed to someone else.
	spec.User = caller.Name
	if spec.Script == "" && len(spec.Args) == 0 {
		fail(w, http.StatusBadRequest, "job has no script or command")
		return
	}
	// Reject what no node in the cluster could ever run, at submit time rather
	// than leaving the job pending forever behind an opaque reason.
	if msg := a.c.ImpossibleRequest(r.Context(), spec.Limits, spec.NodeList, spec.Constraint); msg != "" {
		fail(w, http.StatusBadRequest, msg)
		return
	}
	if _, err := ParseDependency(spec.Dependency); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	tasks, _, err := ParseArray(req.Array)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// The submitter's own limits. Checked here rather than at scheduling so a
	// request that can never be granted is refused with the number it broke,
	// instead of sitting PENDING behind a reason nobody reads.
	if err := a.c.CheckSubmit(r.Context(), caller.Name, spec, len(tasks)); err != nil {
		fail(w, http.StatusForbidden, err.Error())
		return
	}
	a.c.ApplyQoS(r.Context(), caller.Name, &spec)

	NormalizeSpec(&spec)

	// An interactive job has somebody waiting at a terminal, so an array of
	// them is meaningless -- there is one terminal and a hundred tasks.
	if spec.Interactive && len(tasks) > 0 {
		fail(w, http.StatusBadRequest,
			"an interactive job cannot be an array: there is one terminal and many tasks")
		return
	}

	now := time.Now()
	if len(tasks) == 0 {
		spec.ArrayTaskID = -1

		// The relay is registered before the job, because the job's spec has
		// to carry the session id: the node learns which session to attach
		// to from the spec it is handed, and there is no second message.
		var st *stream
		if spec.Interactive {
			st = a.c.NewStream(caller.Name, 0)
			spec.Stream = st.ID
		}
		j, err := a.c.Store().Submit(r.Context(), spec, now)
		if err != nil {
			if st != nil {
				a.c.CloseStream(st.ID)
			}
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if st != nil {
			// Now that the job exists, bind the session to it. This is what
			// the node-side authorisation check compares against.
			a.c.bindStream(st.ID, j.ID)
		}
		if req.Hold {
			a.c.Store().SetHeld(r.Context(), j.ID, true, "waiting for input upload")
			j.Held = true
		}
		v := a.namedJob(view(j, now))
		if st != nil {
			v.Stream = st.ID
		}
		v.Note = a.submitNote(r.Context(), spec)
		writeJSON(w, http.StatusOK, v)
		return
	}

	// Expand server-side so every task is durable before the call returns; a
	// client that dies mid-submit must not leave a half-created array.
	var first *job.Job
	for _, t := range tasks {
		s2 := spec
		s2.ArrayTaskID = t
		j, err := a.c.Store().Submit(r.Context(), s2, now)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if first == nil {
			first = j
		}
		if err := a.c.Store().SetArrayJob(r.Context(), j.ID, first.ID); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	first.ArrayJobID = first.ID
	v := a.namedJob(view(first, now))
	v.ArrayTasks = len(tasks)
	writeJSON(w, http.StatusOK, v)
}

func (a *API) jobs(w http.ResponseWriter, r *http.Request, caller *store.User) {
	user := r.URL.Query().Get("user")
	active := r.URL.Query().Get("active") == "1"
	// A non-admin sees only their own jobs, whatever they ask for.
	if caller.Role != store.RoleAdmin {
		user = caller.Name
	}
	js, err := a.c.Store().List(r.Context(), user, active)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now()
	out := make([]JobView, 0, len(js))
	for _, j := range js {
		out = append(out, a.namedJob(view(j, now)))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) getJob(w http.ResponseWriter, r *http.Request, caller *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad job id")
		return
	}
	j, err := a.c.Store().Get(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if !canTouchJob(caller, j.Spec.User) {
		// 404 rather than 403: existence of another user's job is not
		// something to confirm.
		fail(w, http.StatusNotFound, fmt.Sprintf("job %d not found", id))
		return
	}
	writeJSON(w, http.StatusOK, a.namedJob(view(j, time.Now())))
}

func (a *API) cancel(w http.ResponseWriter, r *http.Request, caller *store.User) {
	id, err := a.ownedJobID(w, r, caller)
	if err != nil {
		return
	}
	// Cancelling an array's id cancels every task in it, as Slurm does.
	if tasks, err := a.c.Store().ArrayTasks(r.Context(), id); err == nil && len(tasks) > 0 {
		n := 0
		for _, t := range tasks {
			if !t.State.Terminal() {
				if err := a.c.Cancel(r.Context(), t.ID); err == nil {
					n++
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "cancelled", "tasks": n})
		return
	}
	if err := a.c.Cancel(r.Context(), id); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

func (a *API) jobAction(w http.ResponseWriter, r *http.Request, caller *store.User, fn func(int64) error) {
	id, err := a.ownedJobID(w, r, caller)
	if err != nil {
		return
	}
	if err := fn(id); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ownedJobID parses the job id and confirms the caller may act on it.
func (a *API) ownedJobID(w http.ResponseWriter, r *http.Request, caller *store.User) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad job id")
		return 0, err
	}
	j, err := a.c.Store().Get(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return 0, err
	}
	if !canTouchJob(caller, j.Spec.User) {
		fail(w, http.StatusNotFound, fmt.Sprintf("job %d not found", id))
		return 0, fmt.Errorf("forbidden")
	}
	return id, nil
}

func (a *API) hold(w http.ResponseWriter, r *http.Request, caller *store.User) {
	a.jobAction(w, r, caller, func(id int64) error { return a.c.Hold(r.Context(), id) })
}
func (a *API) release(w http.ResponseWriter, r *http.Request, caller *store.User) {
	a.jobAction(w, r, caller, func(id int64) error { return a.c.Release(r.Context(), id) })
}
func (a *API) requeue(w http.ResponseWriter, r *http.Request, caller *store.User) {
	a.jobAction(w, r, caller, func(id int64) error { return a.c.Requeue(r.Context(), id) })
}

func (a *API) drain(w http.ResponseWriter, r *http.Request, caller *store.User) {
	reason := r.URL.Query().Get("reason")
	if err := a.c.DrainNode(r.Context(), r.PathValue("name"), reason); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "drained"})
}

func (a *API) resume(w http.ResponseWriter, r *http.Request, caller *store.User) {
	if err := a.c.ResumeNode(r.Context(), r.PathValue("name")); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
}

// removeNode forgets a machine that has left the cluster, so it stops showing
// up as permanently DOWN. An agent calls this on itself during `shome nuke`,
// which is why it must be safe to call for a node that is already gone.
func (a *API) removeNode(w http.ResponseWriter, r *http.Request, caller *store.User) {
	name := r.PathValue("name")
	if err := a.c.RemoveNode(r.Context(), name); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "node": name})
}

func (a *API) stopNode(w http.ResponseWriter, r *http.Request, caller *store.User) {
	if err := a.c.StopNode(r.Context(), r.PathValue("name")); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
}

// token mints a single-use join token for adding a node.
func (a *API) token(w http.ResponseWriter, r *http.Request, caller *store.User) {
	tok, err := a.c.Store().CreateJoinToken(r.Context(), JoinTokenTTL, time.Now())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"token": tok, "expires_in": JoinTokenTTL.String(),
	})
}

// output streams a finished job's captured stdout/stderr.
func (a *API) output(w http.ResponseWriter, r *http.Request, caller *store.User) {
	id, err := a.ownedJobID(w, r, caller)
	if err != nil {
		return
	}
	j, err := a.c.Store().Get(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	ranks := a.c.OutputRanks(id)
	if len(ranks) == 0 {
		if j.State.Terminal() {
			fail(w, http.StatusNotFound,
				fmt.Sprintf("no output stored for job %d (node %s may not have uploaded it yet)", id, j.Node))
		} else {
			fail(w, http.StatusNotFound,
				fmt.Sprintf("job %d is %s; output is archived when it finishes", id, j.State))
		}
		return
	}
	// A specific rank, if asked for.
	if v := r.URL.Query().Get("rank"); v != "" {
		want, err := strconv.Atoi(v)
		if err != nil {
			fail(w, http.StatusBadRequest, "rank must be a number")
			return
		}
		f, err := os.Open(a.c.OutputPathRank(id, want))
		if err != nil {
			fail(w, http.StatusNotFound, fmt.Sprintf("no output for rank %d of job %d", want, id))
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.Copy(w, f)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, rank := range ranks {
		f, err := os.Open(a.c.OutputPathRank(id, rank))
		if err != nil {
			continue
		}
		// Label each rank when there is more than one; unlabelled interleaved
		// output from four machines is unreadable.
		if len(ranks) > 1 {
			fmt.Fprintf(w, "==> rank %d <==\n", rank)
		}
		io.Copy(w, f)
		f.Close()
		if len(ranks) > 1 {
			fmt.Fprintln(w)
		}
	}
}

// uploadStageIn accepts the submitter's input archive.
func (a *API) uploadStageIn(w http.ResponseWriter, r *http.Request, caller *store.User) {
	id, err := a.ownedJobID(w, r, caller)
	if err != nil {
		return
	}
	j, err := a.c.Store().Get(r.Context(), id)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	// Inputs must be in place before the job can start; the CLI submits held
	// and releases after upload, so anything else is a client bug.
	if j.State != job.Pending {
		fail(w, http.StatusConflict,
			fmt.Sprintf("job %d is already %s; inputs must be uploaded before it starts", id, j.State))
		return
	}
	if err := a.c.SaveStage(a.c.StageInPath(id), io.LimitReader(r.Body, maxStageUpload)); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "uploaded"})
}

const maxStageUpload = 2 << 30

func (a *API) downloadStageOut(w http.ResponseWriter, r *http.Request, caller *store.User) {
	id, err := a.ownedJobID(w, r, caller)
	if err != nil {
		return
	}
	f, err := os.Open(a.c.StageOutPath(id))
	if err != nil {
		fail(w, http.StatusNotFound,
			fmt.Sprintf("no staged results for job %d (was it submitted with --stage-out?)", id))
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/gzip")
	io.Copy(w, f)
}

func (a *API) storageList(w http.ResponseWriter, r *http.Request, u *store.User) {
	es, err := a.c.Storage().List(u.Name, r.URL.Query().Get("path"))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, es)
}

func (a *API) storagePut(w http.ResponseWriter, r *http.Request, u *store.User) {
	path := r.URL.Query().Get("path")
	if path == "" {
		fail(w, http.StatusBadRequest, "path is required")
		return
	}
	// Disk is a QoS limit like any other, so it resolves through the same
	// default-plus-override path rather than a separate column on the account.
	// Both sides use the same convention: negative is unlimited, zero means
	// nothing may be stored.
	//
	// The budget is what the account has left *across every machine*, not the
	// whole limit: this directory is one of several holding the same
	// allowance, so checking against the full limit here would let the
	// cluster-wide total be exceeded several times over.
	head, err := a.c.Headroom(r.Context(), u.Name)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := a.c.Storage().Put(u.Name, path, r.Body, head); err != nil {
		// A limit reached is the user's to fix, not a server error.
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "storage limit") ||
			strings.Contains(err.Error(), "allowance") {
			code = http.StatusInsufficientStorage
		}
		fail(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stored"})
}

func (a *API) storageGet(w http.ResponseWriter, r *http.Request, u *store.User) {
	f, err := a.c.Storage().Open(u.Name, r.URL.Query().Get("path"))
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, f)
}

func (a *API) storageDel(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := a.c.Storage().Remove(u.Name, r.URL.Query().Get("path")); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// sshCert issues a short-lived SSH certificate for the caller, so they can
// reach the login node the way they would an academic cluster.
func (a *API) sshCert(w http.ResponseWriter, r *http.Request, u *store.User) {
	var req struct {
		PublicKey string `json:"public_key"`
		PTY       bool   `json:"pty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad request body")
		return
	}
	ca := a.c.UserCA()
	if ca == nil {
		fail(w, http.StatusServiceUnavailable, "SSH access is not configured on this controller")
		return
	}
	cert, err := ca.SignUserKey([]byte(req.PublicKey), u.Name,
		a.c.LoginAccounts(), sshca.UserCertValidity, req.PTY)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.c.Store().Event(r.Context(), 0, "ssh_cert_issued", u.Name, time.Now())
	writeJSON(w, http.StatusOK, map[string]any{
		"certificate": string(cert),
		"valid_for":   sshca.UserCertValidity.String(),
		"principal":   u.Name,
	})
}

// nodesInfo lists every node. Distinct from /node, which reports the cluster
// as a whole for sinfo's summary line.
func (a *API) nodesInfo(w http.ResponseWriter, r *http.Request, _ *store.User) {
	infos, err := a.c.NodeInfos(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Named for display. This handler is registered at both /node and
	// /nodes: they were two identical functions until one of them was given
	// this loop and the other was not, so the CLI kept printing certificate
	// identities while the console printed labels.
	for i := range infos {
		infos[i].Name = a.c.LabelOf(infos[i].Name)
	}
	writeJSON(w, http.StatusOK, infos)
}

// ServiceView is one resident service as the CLI renders it.
type ServiceView struct {
	Name     string `json:"name"`
	Owner    string `json:"owner"`
	Desired  string `json:"desired"`
	JobID    int64  `json:"job_id"`
	JobState string `json:"job_state,omitempty"`
	Node     string `json:"node,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Restarts int    `json:"restarts"`
	Uptime   string `json:"uptime,omitempty"`
}

func (a *API) createService(w http.ResponseWriter, r *http.Request, u *store.User) {
	var req struct {
		Name        string   `json:"name"`
		IdleTimeout string   `json:"idle_timeout"`
		Spec        job.Spec `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad request body")
		return
	}
	if req.Name == "" {
		fail(w, http.StatusBadRequest, "service name is required")
		return
	}
	if req.Spec.Script == "" {
		fail(w, http.StatusBadRequest, "service needs a script to run")
		return
	}
	svc := store.Service{Name: req.Name, Owner: u.Name, Spec: req.Spec}
	if req.IdleTimeout != "" {
		d, err := time.ParseDuration(req.IdleTimeout)
		if err != nil {
			fail(w, http.StatusBadRequest, "bad idle timeout: "+err.Error())
			return
		}
		svc.IdleTimeout = d
	}
	if err := a.c.Store().CreateService(r.Context(), svc, time.Now()); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.c.Store().Event(r.Context(), 0, "service_create", req.Name+" by "+u.Name, time.Now())
	writeJSON(w, http.StatusOK, map[string]string{"name": req.Name, "status": "created"})
}

func (a *API) listServices(w http.ResponseWriter, r *http.Request, u *store.User) {
	owner := ""
	if u.Role != store.RoleAdmin {
		owner = u.Name // a user sees only their own services
	}
	svcs, err := a.c.Store().Services(r.Context(), owner)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now()
	out := make([]ServiceView, 0, len(svcs))
	for _, s := range svcs {
		v := ServiceView{Name: s.Name, Owner: s.Owner, Desired: s.Desired,
			JobID: s.JobID, Endpoint: s.Endpoint, Restarts: s.Restarts}
		if s.JobID != 0 {
			if j, err := a.c.Store().Get(r.Context(), s.JobID); err == nil {
				v.JobState, v.Node = string(j.State), j.Node
				if !j.StartAt.IsZero() && !j.State.Terminal() {
					v.Uptime = job.FormatDuration(now.Sub(j.StartAt))
				}
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// serviceForUser resolves a service the caller is allowed to touch.
func (a *API) serviceForUser(w http.ResponseWriter, r *http.Request, u *store.User) (*store.Service, bool) {
	svc, err := a.c.Store().Service(r.Context(), r.PathValue("name"))
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return nil, false
	}
	if u.Role != store.RoleAdmin && svc.Owner != u.Name {
		fail(w, http.StatusNotFound, "no such service")
		return nil, false
	}
	return svc, true
}

func (a *API) deleteService(w http.ResponseWriter, r *http.Request, u *store.User) {
	svc, ok := a.serviceForUser(w, r, u)
	if !ok {
		return
	}
	// Stop the backing job first; deleting the record alone would orphan it.
	if svc.JobID != 0 {
		a.c.Cancel(r.Context(), svc.JobID)
	}
	if err := a.c.Store().DeleteService(r.Context(), svc.Name); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.c.Store().Event(r.Context(), 0, "service_delete", svc.Name, time.Now())
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (a *API) stopService(w http.ResponseWriter, r *http.Request, u *store.User) {
	svc, ok := a.serviceForUser(w, r, u)
	if !ok {
		return
	}
	a.c.Store().SetServiceDesired(r.Context(), svc.Name, "stopped")
	if svc.JobID != 0 {
		a.c.Cancel(r.Context(), svc.JobID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (a *API) startService(w http.ResponseWriter, r *http.Request, u *store.User) {
	svc, ok := a.serviceForUser(w, r, u)
	if !ok {
		return
	}
	if err := a.c.Store().SetServiceDesired(r.Context(), svc.Name, "running"); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "starting"})
}

// PlanResult is what `shome plan` renders.
type PlanResult struct {
	Feasible bool     `json:"feasible"`
	Why      string   `json:"why"`
	Nodes    []string `json:"nodes,omitempty"`
	Shares   []struct {
		Node   string `json:"node"`
		CPUs   int    `json:"cpus,omitempty"`
		MemMiB int64  `json:"mem_mib,omitempty"`
		GPUs   int    `json:"gpus,omitempty"`
		GPUMiB int64  `json:"gpu_mib,omitempty"`
	} `json:"shares,omitempty"`

	Fabric        string   `json:"fabric,omitempty"`
	FabricWhy     []string `json:"fabric_why,omitempty"`
	FabricOptions []string `json:"fabric_options,omitempty"`
}

// plan answers "can this run, and where" without submitting anything.
//
// Worth its own endpoint: the alternative is submitting a job and reading the
// pending reason, which leaves a job in the queue just to ask a question.
func (a *API) plan(w http.ResponseWriter, r *http.Request, _ *store.User) {
	var spec job.Spec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		fail(w, http.StatusBadRequest, "bad request body")
		return
	}
	req := sched.AggregateFromSpec(spec)
	if req.Empty() {
		fail(w, http.StatusBadRequest,
			"nothing to plan: pass --total-cpus, --total-mem, --total-gpu-mem or --total-gpus")
		return
	}
	nodes, err := a.c.NodeStates(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, why := sched.SolveAggregate(req, nodes)
	out := PlanResult{}
	if p == nil {
		out.Feasible, out.Why = false, why
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.Feasible, out.Why, out.Nodes = true, p.Why, p.NodeNames()
	// Report which coordination backend this would get. An automatic choice
	// the user cannot see is a black box, and the trade-offs differ a lot.
	if len(p.Nodes) > 1 {
		fa := a.c.FabricAllocFor(r.Context(), p.NodeNames())
		if ch, ferr := fabric.Select(fa, FabricWorkloadFor(spec)); ferr == nil {
			out.Fabric, out.FabricWhy, out.FabricOptions = ch.Fabric.Name(), ch.Reasons, ch.Considered
		} else {
			out.Fabric, out.FabricWhy = "none", []string{ferr.Error()}
		}
	}
	for _, sh := range p.Nodes {
		out.Shares = append(out.Shares, struct {
			Node   string `json:"node"`
			CPUs   int    `json:"cpus,omitempty"`
			MemMiB int64  `json:"mem_mib,omitempty"`
			GPUs   int    `json:"gpus,omitempty"`
			GPUMiB int64  `json:"gpu_mib,omitempty"`
		}{sh.Node, sh.CPUs, sh.MemBytes >> 20, sh.GPUs, sh.GPUMem >> 20})
	}
	writeJSON(w, http.StatusOK, out)
}

// NodeInfo is what sinfo renders. It reports enforcement fidelity and what the
// node cannot do, because a node that silently over-promises is worse than one
// that declines to join.
type NodeInfo struct {
	Name        string   `json:"name"`
	State       string   `json:"state"`
	Reason      string   `json:"reason,omitempty"`
	LastSeen    string   `json:"last_seen,omitempty"`
	OS          string   `json:"os"`
	Arch        string   `json:"arch"`
	Tier        string   `json:"tier"`
	CPUs        int      `json:"cpus"`
	UsedCPUs    int      `json:"used_cpus"`
	MemMiB      int64    `json:"mem_mib"`
	UsedMemMiB  int64    `json:"used_mem_mib"`
	GPUs        int      `json:"gpus"`
	MemLimit    string   `json:"mem_enforcement"`
	CPULimit    string   `json:"cpu_enforcement"`
	Limitations []string `json:"limitations,omitempty"`
}

// shutdown asks the daemon to stop. Running jobs are left alone: they are
// independent process trees, and the controller reconciles on restart.
func (a *API) shutdown(w http.ResponseWriter, r *http.Request, caller *store.User) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "shutting down"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		a.c.RequestStop()
	}()
}

func (a *API) events(w http.ResponseWriter, r *http.Request, caller *store.User) {
	n := 50
	if v := r.URL.Query().Get("n"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			n = p
		}
	}
	evs, err := a.c.Store().Events(r.Context(), n)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, evs)
}

// maxUnixPath is the sun_path limit. It is 104 on macOS/BSD and 108 on Linux;
// use the smaller so behaviour does not differ by platform. Exceeding it fails
// with a bare "invalid argument" from bind(2), which is impossible to diagnose
// without knowing this limit exists.
const maxUnixPath = 104

// Serve listens on a unix socket at path until the listener is closed.
func Serve(c *Controller, path string) (*http.Server, net.Listener, error) {
	ln, err := SocketListener(path)
	if err != nil {
		return nil, nil, err
	}
	api := &API{c: c}
	srv := &http.Server{Handler: api.routes(), ReadHeaderTimeout: 5 * time.Second}
	return srv, ln, nil
}

// ServeClientProxy serves the control socket on a machine that is not the
// controller, handing what arrives to h.
//
// A node is not a controller and has no API of its own, so the CLI on one
// had nothing to talk to: `squeue` on a machine that had joined a cluster
// reported that shomectld was not running, which is true and unhelpful --
// the cluster it had joined was running perfectly well somewhere else.
//
// The socket is the same path with the same owner-only permissions, so the
// CLI needs to know nothing about which kind of machine it is on. What goes
// over it still needs an account's token: the node adds no authority of its
// own, and the handler it is given forwards to the controller, which decides.
func ServeClientProxy(path string, h http.Handler) (*http.Server, net.Listener, error) {
	ln, err := SocketListener(path)
	if err != nil {
		return nil, nil, err
	}
	return &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}, ln, nil
}

// SocketListener opens the control socket, refusing when a live daemon
// already holds it.
func SocketListener(path string) (net.Listener, error) {
	if len(path) >= maxUnixPath {
		return nil, fmt.Errorf(
			"control socket path is %d bytes, over the %d-byte OS limit: %s\n"+
				"set SHOME_ROOT to a shorter path", len(path), maxUnixPath, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// A stale socket from an unclean shutdown blocks Listen and must be
	// removed -- but only once we know it is genuinely stale.
	//
	// Probing by connecting first is essential: blindly removing it would
	// destroy a LIVE controller's socket, leaving that process running and
	// unreachable. Worse, both would then write the same SQLite database.
	// Refusing to start is the only safe answer.
	if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		if c, derr := net.DialTimeout("unix", path, 2*time.Second); derr == nil {
			c.Close()
			// Single line: this surfaces through a structured logger, which
			// escapes embedded newlines into literal \n and makes multi-line
			// advice unreadable.
			dir := filepath.Dir(path)
			return nil, fmt.Errorf(
				"another shome daemon is already running on %s -- stop it with "+
					"'SHOME_ROOT=%s shome down', or use a different -root "+
					"for a second cluster", dir, dir)
		}
		os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// Owner-only: filesystem permissions are the access control in M1.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// SocketPath is the default control socket location.
func SocketPath(root string) string { return filepath.Join(root, "shome.sock") }

// DefaultRoot is where shome keeps its state.
//
// Deliberately OUTSIDE the owner's home. Every job profile denies the owner's
// home, and job scratch lives under this root -- putting it in ~ would mean
// carving the scratch back out of a deny that also covers the user's real
// files, which is fragile and easy to get wrong. /Users/Shared is sticky-bit
// world-writable on macOS, so this needs no privilege; the root itself is
// created 0700.

// pointerPath is where a running controller records its state directory.
func pointerPath() string { return pointerPathNamed("controller") }

// nodePointerPath is the equivalent for a worker agent, so `shome status`,
// `pause` and `resume` work on a node without SHOME_ROOT in every shell.
func nodePointerPath() string { return pointerPathNamed("node") }

func pointerPathNamed(kind string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".shome", kind)
}

// RootIsExplicit reports whether the state directory came from SHOME_ROOT
// rather than from this machine's own installation.
//
// The pointer files answer "where does this machine keep shome", so a run
// that was *told* where to look must not rewrite them: starting a throwaway
// second cluster with SHOME_ROOT set would otherwise silently redirect every
// later command -- including 'shome nuke' -- at the temporary directory
// instead of the real installation.
func RootIsExplicit() bool { return os.Getenv("SHOME_ROOT") != "" }

// WriteNodePointer records a worker agent's state directory.
func WriteNodePointer(root string) {
	if RootIsExplicit() {
		return
	}
	p := nodePointerPath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	os.WriteFile(p, []byte(root+"\n"), 0o600)
}

// readNodePointer returns a recorded agent root that still looks real.
func readNodePointer() string {
	p := nodePointerPath()
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	root := strings.TrimSpace(string(b))
	if root == "" {
		return ""
	}
	if _, err := os.Stat(root); err != nil {
		return ""
	}
	return root
}

// WriteControllerPointer records this controller's root for CLI discovery.
// Best-effort: a controller that cannot write it still works, callers just
// need SHOME_ROOT.
func WriteControllerPointer(root string) {
	if RootIsExplicit() {
		return
	}
	p := pointerPath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	os.WriteFile(p, []byte(root+"\n"), 0o600)
}

// ClearControllerPointer removes the pointer if it still names root, so a
// clean shutdown does not leave the CLI aimed at a directory that is gone.
//
// Reads the file directly rather than via readControllerPointer: by the time
// this runs the socket is already gone, so the liveness check that function
// applies would report no pointer and the stale file would survive.
func ClearControllerPointer(root string) {
	p := pointerPath()
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	if strings.TrimSpace(string(b)) == root {
		os.Remove(p)
	}
}

// readControllerPointer returns the recorded root, but only if it still looks
// like a live controller. A stale pointer must not shadow the platform
// default.
func readControllerPointer() string {
	p := pointerPath()
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	root := strings.TrimSpace(string(b))
	if root == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(root, "shome.sock")); err != nil {
		return ""
	}
	return root
}

func DefaultRoot() string {
	if v := os.Getenv("SHOME_ROOT"); v != "" {
		return v
	}
	// A running controller records where it lives, so the CLI finds it from
	// any shell. Without this, SHOME_ROOT has to be exported in every new
	// terminal, and forgetting reads as "the controller is not running" --
	// which is both wrong and hard to guess from.
	if v := readControllerPointer(); v != "" {
		return v
	}
	// Then a worker agent's root, so owner commands work on a node too. The
	// controller wins when a machine is both.
	if v := readNodePointer(); v != "" {
		return v
	}
	if runtime.GOOS == "darwin" {
		if fi, err := os.Stat("/Users/Shared"); err == nil && fi.IsDir() {
			u := os.Getenv("USER")
			if u == "" {
				u = "default"
			}
			return filepath.Join("/Users/Shared", "shome-"+u)
		}
	}
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "shome")
	}
	return filepath.Join(os.TempDir(), "shome")
}

// NormalizeSpec fills in what a job's request implies but does not say.
//
// One place, applied to every submission whatever asked for it, so the CLI
// and the API cannot end up with different defaults.
func NormalizeSpec(spec *job.Spec) {
	// A shell gets network, the same as a login session and for the same
	// reason: a package manager that cannot reach an index is not a package
	// manager, and installing things is most of what somebody does at a
	// prompt.
	//
	// It grants nothing new. The account already has the network from its
	// login session on the same cluster with the same credentials, so
	// withholding it here bought nothing and cost the worst error in the
	// system -- uv reporting "dns error: nodename nor servname provided",
	// which names neither shome nor the flag that would have fixed it.
	//
	// Batch keeps the deny-by-default: nobody is watching one, and a script
	// that reaches the network should say so with --network.
	if spec.PTY {
		spec.Limits.Network = true
	}
}
