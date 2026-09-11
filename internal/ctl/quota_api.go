package ctl

import (
	"net/http"

	"github.com/davidwu/shome/internal/fairshare"
	"github.com/davidwu/shome/internal/qos"
	"github.com/davidwu/shome/internal/store"
)

// "What am I using, and what am I allowed?" in one answer.
//
// The pieces already existed and were spread across three commands: `shome
// admin qos show NAME` for the limits, `shome fs du` for storage, `sshare`
// for fair-share standing. None of them answers the question on its own, and
// two of the three read as administrative.
//
// Assembled here rather than in the CLI so the console and any other client
// get the same figures, and so "used" and "limit" are computed side by side
// -- reading them from separate endpoints is how they end up inconsistent.

// QuotaLine is one measured thing, with its limit if it has one.
type QuotaLine struct {
	Name string `json:"name"`
	// Used and Limit are in the unit named by Unit. Limit is negative when
	// there is no limit, which is not the same as a limit of zero -- zero
	// means none is allowed, and a user needs to be able to tell those apart.
	Used  int64  `json:"used"`
	Limit int64  `json:"limit"`
	Unit  string `json:"unit"`
}

// Percent is how much of the allowance is in use, or -1 when unlimited.
func (q QuotaLine) Percent() float64 {
	if q.Limit < 0 || q.Limit == 0 {
		return -1
	}
	return float64(q.Used) / float64(q.Limit) * 100
}

// QuotaView is everything an account is using and allowed.
type QuotaView struct {
	User string `json:"user"`

	// Compute is what is in use right now and the ceiling on it.
	Compute []QuotaLine `json:"compute"`
	// PerJob are ceilings on a single job rather than on a total, so they
	// have no "used" figure and are reported separately -- showing them in
	// the same table with a blank usage column reads as a measurement that
	// failed.
	PerJob []QuotaLine `json:"per_job"`

	// Storage is per machine, because a file lives on one machine and the
	// same path on two machines is two files.
	Storage    FSUsage `json:"storage"`
	StorageOld bool    `json:"storage_stale,omitempty"`

	// Share is the account's fair-share standing, when the cluster orders
	// its queue by priority. Omitted when it does not, because a number
	// nothing acts on invites the reader to act on it.
	Share        *fairshare.Share `json:"share,omitempty"`
	PriorityOn   bool             `json:"priority_on"`
	PriorityHint string           `json:"priority_hint,omitempty"`
}

// quota reports one account's usage against its limits.
func (a *API) quotaFor(w http.ResponseWriter, r *http.Request, caller *store.User) {
	// An admin may ask about somebody else; anyone else gets themselves.
	user := caller.Name
	if q := r.URL.Query().Get("user"); q != "" && caller.Role != store.RoleUser {
		user = q
	}
	if _, err := a.c.Store().UserByName(r.Context(), user); err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}

	eff, err := a.c.EffectiveQoS(r.Context(), user)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := QuotaView{User: user}

	used, uerr := a.c.RunningUsage(r.Context(), user)
	submitted, serr := a.c.Store().CountActiveForUser(r.Context(), user)
	if uerr == nil && serr == nil {
		out.Compute = []QuotaLine{
			{"jobs queued and running", int64(submitted), int64(eff.MaxSubmittedJobs), "jobs"},
			{"jobs running", int64(used.Jobs), int64(eff.MaxRunningJobs), "jobs"},
			{"cpu cores in use", int64(used.CPUs), int64(eff.MaxRunningCPUs), "cores"},
			{"gpus in use", int64(used.GPUs), int64(eff.MaxRunningGPUs), "gpus"},
			{"memory in use", used.MemMB, eff.MaxRunningMemMB, "MiB"},
		}
	}
	out.PerJob = []QuotaLine{
		{"cpu cores per job", 0, int64(eff.MaxCPUsPerJob), "cores"},
		{"gpus per job", 0, int64(eff.MaxGPUsPerJob), "gpus"},
		{"memory per job", 0, eff.MaxMemMBPerJob, "MiB"},
		{"processes per job", 0, int64(eff.MaxProcsPerJob), "procs"},
		{"walltime per job", 0, int64(eff.MaxWalltime / 1e9), "seconds"},
	}

	out.Storage = a.usageFor(r, user)
	for _, n := range out.Storage.Nodes {
		if n.Stale {
			out.StorageOld = true
		}
	}

	pcfg := a.c.PriorityConfig()
	out.PriorityOn = pcfg.Enabled
	if pcfg.Enabled {
		if s, err := a.c.ShareOf(r.Context(), user); err == nil {
			out.Share = &s
			out.PriorityHint = shareHint(s)
		}
	} else {
		out.PriorityHint = "Jobs run in submission order; recent usage does not affect your place in the queue."
	}
	writeJSON(w, http.StatusOK, out)
}

// shareHint says what a fair-share factor means for the reader, because the
// number alone tells somebody who has not read the documentation nothing.
func shareHint(s fairshare.Share) string {
	switch {
	case s.RawUsage <= 0:
		return "You have not used the cluster recently, so your jobs go ahead of those who have."
	case s.Factor >= 0.75:
		return "You have used well under your share, so your jobs go ahead of most others'."
	case s.Factor >= 0.4:
		return "You are using about your share. Jobs are ordered normally."
	case s.Factor >= 0.15:
		return "You have used more than your share recently, so your jobs wait behind lighter users'."
	}
	return "You have used much more than your share recently, so your jobs wait behind others'. " +
		"This fades as your recent usage decays."
}

var _ = qos.Unlimited
