package ctl

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/davidwu/shome/internal/job"
)

// The cluster API, as reached from inside a running job.
//
// # Why a job needs one at all
//
// `squeue` from a compute node is not a luxury. A job that is one task of an
// array wants to know what the rest are doing; a job about to write a large
// result wants to know its quota; a job that has just started wants to know
// what else is on the machine. On a real cluster all of that works from a
// compute node, and a person who has to exit the job and ask from somewhere
// else has been handed a worse computer than the one they were promised.
//
// # Why it is routed this way
//
// The controller's user API listens on a unix socket, owner-only, on the
// controller's own machine. A compute node is usually not that machine, and
// a job is in a sandbox that denies the installation root even when it is.
// So neither the socket nor a second public listener is the answer: the
// first is unreachable, and the second would put the whole cluster API on
// the network for the sake of jobs.
//
// What already exists is the node's own connection to the controller --
// mutually authenticated, already open, already the way output and
// keystrokes travel. So the same API is served over it, at /agent/api, and
// the node forwards a job's requests through it. Two credentials are
// involved and they answer different questions: the node's certificate says
// which machine is asking, and the job's token says who to answer as. The
// node cannot escalate by forwarding, because forwarding without a token
// gets the same 401 as anyone else.

// JobTokenGrace is how long a job's credential outlives its time limit.
//
// Not zero: a job is killed at its limit and the last thing it does may be
// to report, and a credential that expired at the same instant would turn
// that into an authentication error. Not long, because this is the bound
// that applies if every other way of cleaning it up fails.
const JobTokenGrace = time.Hour

// JobTokenTTL is the lifetime for a job with no time limit of its own.
const JobTokenTTL = 7 * 24 * time.Hour

// jobSession names the credential a job holds, so it can be recognised and
// revoked as a job's rather than a login session's.
func jobSession(id int64) string { return fmt.Sprintf("job-%d", id) }

// jobToken issues the credential a job uses to talk to the cluster.
//
// Scoped to the account the job belongs to -- not to the node, which must
// not gain the ability to act as anybody, and not the account's own token,
// which is stored hashed and cannot be handed out. Empty when there is no
// account to scope it to or the store refuses, which costs the job the
// cluster commands and nothing else.
func (c *Controller) jobToken(ctx context.Context, j *job.Job) string {
	if j == nil || j.Spec.User == "" {
		return ""
	}
	ttl := JobTokenTTL
	if w := j.Spec.Limits.Walltime; w > 0 {
		ttl = w + JobTokenGrace
	}
	tok, err := c.store.CreateSessionToken(ctx, j.Spec.User, jobSession(j.ID), ttl, c.now())
	if err != nil {
		c.log.Warn("no credential for this job; the cluster commands will not "+
			"work inside it", "job", j.ID, "err", err)
		return ""
	}
	return tok
}

// reapJobTokens deletes the credentials of jobs that have finished.
//
// A sweep rather than a call beside every place a job ends. There are eight
// of those -- a report, a cancellation, a time limit, a lost node, a failed
// launch -- and a credential that outlives its job because one of them was
// missed is exactly the kind of thing nobody notices. The expiry on the
// token is the backstop; this is what makes the ordinary case immediate.
func (c *Controller) reapJobTokens(ctx context.Context) {
	if n, err := c.store.DeleteFinishedJobTokens(ctx); err != nil {
		c.log.Debug("could not clear finished jobs' credentials", "err", err)
	} else if n > 0 {
		c.log.Debug("cleared credentials for finished jobs", "count", n)
	}
}

// jobAPI serves the cluster API to a job, over the node's own connection.
//
// The node is authenticated by its certificate, as every agent endpoint is.
// That is all its identity buys: the request is then handled exactly as one
// arriving on the control socket, which means it needs the job's own token
// and gets that account's answers and no more.
func (s *AgentServer) jobAPI(w http.ResponseWriter, r *http.Request) {
	if node := requirePeer(w, r); node == "" {
		return
	}
	http.StripPrefix("/agent/api", s.api).ServeHTTP(w, r)
}
