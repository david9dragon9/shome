package ctl

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/fairshare"
	"github.com/davidwu/shome/internal/store"
)

// The priority API. One read for everybody, one write for admins, and the
// per-account entitlement separately -- the same division as QoS, because it
// is the same shape of thing.

// PriorityView is the policy and what it currently implies.
type PriorityView struct {
	Config fairshare.Config   `json:"config"`
	Shares []fairshare.Share  `json:"shares,omitempty"`
	Scores map[string]float64 `json:"scores,omitempty"`
	File   string             `json:"file"`
	// Explain says in one line what the cluster is doing, so a reader does
	// not have to interpret the flags.
	Explain string `json:"explain"`
}

// getPriority reports the policy.
//
// Readable by any account, not just admins: a user who wants to know why
// their job is behind somebody else's has to be able to see the rules. The
// rules are not a secret; the weights are the whole point of publishing them.
func (a *API) getPriority(w http.ResponseWriter, r *http.Request, caller *store.User) {
	cfg := a.c.PriorityConfig()
	out := PriorityView{Config: cfg, File: fairshare.File(a.c.root),
		Explain: explainPriority(cfg)}

	shares, err := a.c.Shares(r.Context())
	if err == nil {
		if caller.Role == store.RoleUser {
			// A plain user sees their own standing. Another account's usage
			// is not their business, and publishing the whole table would
			// make everyone's activity visible to everyone.
			for _, s := range shares {
				if s.User == caller.Name {
					out.Shares = []fairshare.Share{s}
					break
				}
			}
		} else {
			out.Shares = shares
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// explainPriority describes the policy in a sentence.
func explainPriority(c fairshare.Config) string {
	if !c.Enabled {
		return "Priority is off: jobs run in submission order. " +
			"Turn it on with 'shome admin priority set enabled true'."
	}
	var parts []string
	if c.Weights.FairShare > 0 {
		parts = append(parts, fmt.Sprintf("recent usage (weight %g, half-life %s)",
			c.Weights.FairShare, c.HalfLife))
	}
	if c.Weights.Age > 0 {
		parts = append(parts, fmt.Sprintf("time waited (weight %g, full after %s)",
			c.Weights.Age, c.MaxAge))
	}
	if c.Weights.Size > 0 {
		which := "larger"
		if c.FavorSmall {
			which = "smaller"
		}
		parts = append(parts, fmt.Sprintf("job size, favouring %s (weight %g)",
			which, c.Weights.Size))
	}
	s := "Jobs are ordered by " + strings.Join(parts, ", ") + "."
	if c.Backfill {
		s += fmt.Sprintf(" Backfill is on: a shorter job may use a gap held for a "+
			"waiting job, if it will hand it back in time (%d held at once).",
			c.BackfillDepth)
	} else {
		s += " Backfill is off, so a job that cannot start blocks the queue behind it."
	}
	return s
}

// setPriority replaces the policy.
//
// Whole-document rather than field-by-field, because the console edits a form
// and the CLI edits one setting at a time on top of a fetched document -- and
// a partial update would need every field to distinguish "unset" from "zero",
// which for a set of weights is exactly the distinction that matters.
func (a *API) setPriority(w http.ResponseWriter, r *http.Request, caller *store.User) {
	var req fairshare.Config
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	// Shares are edited through their own endpoint, so an admin saving the
	// form cannot accidentally drop every entitlement by omitting the map.
	cur := a.c.PriorityConfig()
	req.Shares = cur.Shares
	if err := validPriority(req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.c.SetPriorityConfig(req.WithDefaults()); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.c.log.Info("priority policy changed", "admin", caller.Name,
		"enabled", req.Enabled, "backfill", req.Backfill)
	a.c.Store().Event(r.Context(), 0, "priority", explainPriority(req.WithDefaults()), time.Now())
	writeJSON(w, http.StatusOK, PriorityView{
		Config: req.WithDefaults(), File: fairshare.File(a.c.root),
		Explain: explainPriority(req.WithDefaults()),
	})
}

// validPriority refuses a policy that would not do anything sensible.
func validPriority(c fairshare.Config) error {
	if c.Weights.FairShare < 0 || c.Weights.Age < 0 || c.Weights.Size < 0 {
		return fmt.Errorf("weights cannot be negative")
	}
	// Every weight zero with priority on would score every job identically,
	// which is submission order wearing a disguise. Refusing says so rather
	// than leaving an admin to wonder why nothing changed.
	if c.Enabled && c.Weights.Sum() == 0 {
		return fmt.Errorf("every weight is zero, so every job would score the same.\n" +
			"Set at least one of weight-fairshare, weight-age or weight-size, " +
			"or turn priority off")
	}
	if c.HalfLife < 0 || c.MaxAge < 0 {
		return fmt.Errorf("half-life and max-age cannot be negative")
	}
	if c.Resource.GPU < 0 || c.Resource.MemGB < 0 {
		return fmt.Errorf("resource weights cannot be negative")
	}
	if c.DefaultShares < 0 {
		return fmt.Errorf("default shares cannot be negative")
	}
	if c.BackfillDepth < 0 {
		return fmt.Errorf("backfill depth cannot be negative")
	}
	return nil
}

// setUserShares changes one account's entitlement.
func (a *API) setUserShares(w http.ResponseWriter, r *http.Request, caller *store.User) {
	name := r.PathValue("name")
	var req struct {
		Shares *float64 `json:"shares"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	if req.Shares == nil {
		fail(w, http.StatusBadRequest, "shares is required (0 restores the default)")
		return
	}
	if *req.Shares < 0 {
		fail(w, http.StatusBadRequest, "shares cannot be negative")
		return
	}
	if err := a.c.SetUserShares(r.Context(), name, *req.Shares); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.c.log.Info("shares changed", "admin", caller.Name, "user", name, "shares", *req.Shares)
	s, _ := a.c.ShareOf(r.Context(), name)
	writeJSON(w, http.StatusOK, s)
}

// shareInfo is what `sshare` prints.
func (a *API) shareInfo(w http.ResponseWriter, r *http.Request, caller *store.User) {
	who := r.URL.Query().Get("user")
	// No account named means the caller's own, for an admin as much as for
	// anyone else -- that is what sshare does, and an admin who wants the
	// whole table asks for it with -a. Defaulting an admin to everyone would
	// make the common case the noisy one.
	if who == "" {
		who = caller.Name
	}
	// A plain user only ever sees themselves, whatever they ask for.
	if caller.Role == store.RoleUser {
		who = caller.Name
	}
	cfg := a.c.PriorityConfig()
	if who != "all" {
		s, err := a.c.ShareOf(r.Context(), who)
		if err != nil {
			fail(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": cfg.Enabled, "shares": []fairshare.Share{s},
			"explain": explainPriority(cfg),
		})
		return
	}
	shares, err := a.c.Shares(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": cfg.Enabled, "shares": shares, "explain": explainPriority(cfg),
	})
}

// jobPriority reports one pending job's score and how it was arrived at.
func (a *API) jobPriority(w http.ResponseWriter, r *http.Request, caller *store.User) {
	id, err := a.ownedJobID(w, r, caller)
	if err != nil {
		return
	}
	s, ok := a.c.ScoreOf(id)
	if !ok {
		cfg := a.c.PriorityConfig()
		msg := "this job has no priority score"
		if !cfg.Enabled {
			msg = "priority is off; jobs run in submission order"
		} else {
			msg += " -- it is not pending, or has not been through a scheduling pass yet"
		}
		writeJSON(w, http.StatusOK, map[string]any{"scored": false, "explain": msg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scored": true, "job_id": id, "priority": s.Priority,
		"fair": s.Fair, "age": s.Age, "size": s.Size,
		"explain": s.Describe(),
	})
}

// parseShares reads a share count from a query string, for the CLI.
func parseShares(v string) (float64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", v)
	}
	return f, nil
}
