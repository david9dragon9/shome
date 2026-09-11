// Package agentapi defines the wire contract between the controller and its
// node agents.
//
// The protocol is deliberately agent-initiated: agents dial out to the
// controller and poll, rather than the controller connecting to them. Home
// machines sit behind NAT, change networks, sleep and wake, and rarely have
// stable addresses. An agent that reconnects on its own needs no port
// forwarding and recovers from a network change without intervention.
package agentapi

import (
	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/platform"
)

// JoinRequest is the one call an agent makes before it has a certificate.
type JoinRequest struct {
	Token string `json:"token"`
	Node  string `json:"node"`
}

// JoinResponse carries the agent's newly minted identity.
type JoinResponse struct {
	CACert   string `json:"ca_cert"`
	NodeCert string `json:"node_cert"`
	NodeKey  string `json:"node_key"`
}

// JobStatus is an agent's report on one job it is running or has finished.
type JobStatus struct {
	// Refused means the node declined to start the job at all (owner policy),
	// so the controller should requeue rather than record a failure.
	Refused bool `json:"refused,omitempty"`

	ID       int64     `json:"id"`
	State    job.State `json:"state"`
	ExitCode int       `json:"exit_code"`
	PeakMem  int64     `json:"peak_mem"`
	Reason   string    `json:"reason"`
}

// Heartbeat is sent on every poll: it is liveness, inventory and job reporting
// in one message, so there is exactly one thing that can fail.
type Heartbeat struct {
	// Addr is how other agents can reach this node. Self-reported because the
	// controller's view of the connection is not routable between peers.
	Addr string `json:"addr,omitempty"`

	// OwnerAction is the machine owner's current disposition toward cluster
	// work: run, throttle, suspend or drain. The controller must respect it --
	// a cluster admin cannot override a node's owner.
	OwnerAction string `json:"owner_action,omitempty"`
	OwnerReason string `json:"owner_reason,omitempty"`

	Node string                `json:"node"`
	Caps platform.Capabilities `json:"caps"`
	Jobs []JobStatus           `json:"jobs"`

	// Telemetry is what the machine is actually doing, as distinct from what
	// the scheduler has allocated on it. Carried on the heartbeat rather than
	// polled separately so that liveness and utilisation cannot disagree: if
	// a reading arrived, the node was up when it was taken.
	Telemetry platform.Telemetry `json:"telemetry"`

	// Live is per-job resource use for jobs running right now. PeakMem in
	// Jobs is the enforcement number; this is the dashboard number.
	Live []JobLive `json:"live,omitempty"`

	// Claimed is every job this agent is supervising or still has a result
	// for: the complete answer to "what are you running?".
	//
	// The controller needs it to notice a job that no longer exists anywhere.
	// An agent that restarts -- because the machine rebooted, or because
	// somebody ran `shome restart` -- comes back with no memory of what it
	// was running, while the controller's record still says RUNNING. Without
	// this the job stays RUNNING for as long as the node keeps heartbeating,
	// which is forever: nothing supervises it, no limit is enforced on it,
	// and no result ever arrives.
	Claimed []int64 `json:"claimed,omitempty"`

	// ClaimsOK reports that Claimed is complete, so the controller may treat
	// a job it does not mention as gone.
	//
	// Separate for the reason StorageOK is: an empty list means either "I am
	// running nothing" or "this agent is too old to say", and acting on the
	// second as though it were the first would fail every running job in the
	// cluster the first time an old agent checked in.
	ClaimsOK bool `json:"claims_ok,omitempty"`

	// Storage is what each account occupies on this machine, measured from
	// the filesystem rather than inferred from transfers -- the figure a
	// limit is enforced against must not come from a record of what was
	// meant to happen.
	Storage []UserStorage `json:"storage,omitempty"`

	// StorageOK reports that Storage is this machine's complete current view
	// of who has what, so the controller may treat an account it does not
	// mention as having nothing here.
	//
	// Needed because "no accounts have files on me" and "I have not managed
	// to look" are the same empty list, and treating the second as the first
	// would discard a machine's usage; treating the first as the second would
	// charge people for files they deleted.
	StorageOK bool `json:"storage_ok,omitempty"`

	// StorageAt is when the measurement behind Storage began, in unix
	// milliseconds.
	//
	// Needed because the report is applied wholesale -- a file the machine
	// no longer lists has gone -- while the walk it comes from takes time:
	// on a home directory with a Python environment in it, tens of
	// thousands of files. A file that arrived during the walk is missing
	// from the report through no fault of anybody's, and replacing the
	// index with that report deleted the record of it. The controller
	// compares this against what it has been told since.
	StorageAt int64 `json:"storage_at,omitempty"`

	// Transfers reports file operations this agent has finished.
	Transfers []TransferResult `json:"transfers,omitempty"`
}

// UserStorage is one account's files on the reporting machine.
type UserStorage struct {
	User      string      `json:"user"`
	Bytes     int64       `json:"bytes"`
	Files     int         `json:"files"`
	Truncated bool        `json:"truncated,omitempty"`
	Entries   []FileEntry `json:"entries,omitempty"`
}

// FileEntry is one indexed file.
type FileEntry struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // unix milliseconds
}

// TransferResult is the outcome of one file action.
type TransferResult struct {
	Transfer int64  `json:"transfer"`
	Kind     string `json:"kind"`
	OK       bool   `json:"ok"`
	Bytes    int64  `json:"bytes,omitempty"`
	Error    string `json:"error,omitempty"`
}

// JobLive is a running job's current resource use on its node.
type JobLive struct {
	ID         int64   `json:"id"`
	MemBytes   int64   `json:"mem_bytes"`
	PeakBytes  int64   `json:"peak_bytes"`
	NProcs     int     `json:"nprocs"`
	CPUPercent float64 `json:"cpu_percent"`
	Throttled  bool    `json:"throttled,omitempty"`
	ElapsedSec float64 `json:"elapsed_sec"`
}

// ActionKind is what the controller wants the agent to do next.
type ActionKind string

const (
	ActionLaunch ActionKind = "launch"
	ActionKill   ActionKind = "kill"

	// ActionStop asks the agent to exit cleanly. Without it, taking a node out
	// of service would mean logging into that machine, which defeats the point
	// of a cluster. Running jobs are left alone; they are separate process
	// trees and are reconciled when an agent returns.
	ActionStop ActionKind = "stop"

	// File actions move a user's files between machines, via the controller.
	//
	// Relayed rather than peer-to-peer because agents dial out and cannot be
	// dialled: a node behind NAT is reachable only by the connection it
	// opened. The controller is the one place both ends can always reach, so
	// a copy is an upload followed by a download, and the cost is a round
	// trip through it.
	ActionFileSend   ActionKind = "file-send"   // upload a file to the controller
	ActionFileRecv   ActionKind = "file-recv"   // download a file from the controller
	ActionFileRemove ActionKind = "file-remove" // delete a user's file here
)

// Action is one instruction. The controller decides; the agent executes.
type Action struct {
	Kind  ActionKind `json:"kind"`
	JobID int64      `json:"job_id"`
	Spec  *job.Spec  `json:"spec,omitempty"`

	// Gang wiring, set only for multi-node jobs. The agent turns these into
	// the environment a distributed runtime expects.
	Rank       int      `json:"rank,omitempty"`
	NNodes     int      `json:"nnodes,omitempty"`
	NodeList   []string `json:"node_list,omitempty"`
	MasterAddr string   `json:"master_addr,omitempty"`
	MasterPort int      `json:"master_port,omitempty"`

	// Fabric wiring: what this rank actually runs.
	Fabric    string            `json:"fabric,omitempty"`
	Role      string            `json:"role,omitempty"`
	FabricEnv map[string]string `json:"fabric_env,omitempty"`
	Command   []string          `json:"command,omitempty"`
	Pre       [][]string        `json:"pre,omitempty"`
	Post      [][]string        `json:"post,omitempty"`

	// Token is the job's own credential, for launch actions.
	//
	// It is how a job runs `squeue` or `squota`: scoped to the account the
	// job belongs to, valid while the job is, and never written to the job
	// record -- it travels with the instruction to start the job and no
	// further. A job on a compute node that cannot ask the cluster anything
	// is a job whose owner cannot check on their own array.
	Token string `json:"token,omitempty"`

	// File transfer, set for the file actions. Transfer is the controller's
	// handle for the operation; the agent quotes it back so a result can be
	// matched to a request even after a reconnect.
	Transfer  int64  `json:"transfer,omitempty"`
	FileUser  string `json:"file_user,omitempty"`
	FilePath  string `json:"file_path,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
	// Limit is how many bytes this agent may write for the receiving side,
	// which is the account's remaining cluster-wide allowance at the moment
	// the transfer was authorised.
	Limit int64 `json:"limit,omitempty"`
}

// HeartbeatResponse returns work for the agent.
type HeartbeatResponse struct {
	Actions []Action `json:"actions"`

	// Known is false when the controller has never heard of this node, which
	// happens if its database was reset. The agent re-joins rather than
	// heartbeating forever into a void.
	Known bool `json:"known"`

	// Acked lists terminal job reports the controller has durably recorded.
	// Anything not acked is resent, so a dropped connection cannot lose a
	// finished job's result.
	Acked []int64 `json:"acked,omitempty"`

	// Cluster is what this cluster is called, and Label what the admin calls
	// this machine.
	//
	// Sent on every heartbeat rather than at join, so renaming either takes
	// effect within a heartbeat instead of at the next restart. The agent
	// needs them to build the prompt for an interactive job: only the
	// controller knows the cluster's name, and only it holds the admin's
	// labels for machines.
	Cluster string `json:"cluster,omitempty"`
	Label   string `json:"label,omitempty"`
}
