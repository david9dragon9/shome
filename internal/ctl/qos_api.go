package ctl

import (
	"encoding/json"
	"net/http"

	"github.com/davidwu/shome/internal/qos"
	"github.com/davidwu/shome/internal/store"
)

// The QoS API. Four questions, because there are four things to know: what
// the cluster as a whole will hand out, what each account gets by default,
// what this account had changed, and what therefore applies to them.

type qosView struct {
	// Cluster is the aggregate ceiling across everybody; Defaults is what
	// each account gets; Overrides is what this one account had changed.
	Cluster   qos.Limits        `json:"cluster"`
	Defaults  qos.Limits        `json:"defaults"`
	Overrides qos.Limits        `json:"overrides,omitempty"`
	Effective map[string]string `json:"effective"`
	Usage     map[string]any    `json:"usage,omitempty"`
	File      string            `json:"file"`
}

// getQoS reports the limits in force, for the caller or for a named account.
func (a *API) getQoS(w http.ResponseWriter, r *http.Request, caller *store.User) {
	user := r.URL.Query().Get("user")
	// A plain user may ask about themselves. Their own limits are not a
	// secret -- they need to know why a job was refused -- but somebody
	// else's are not their business.
	if caller.Role == store.RoleUser || user == "" {
		user = caller.Name
	}

	cfg := a.c.QoSConfig()
	def := cfg.PerUser

	// "cluster" is not an account: it asks for the aggregate ceiling and what
	// the whole cluster is currently using against it.
	if user == "cluster" && caller.Role != store.RoleUser {
		eff := cfg.ClusterEffective()
		shown := map[string]string{}
		for _, f := range qos.Fields {
			shown[f.Name] = f.Get(eff)
		}
		v := qosView{Cluster: cfg.Cluster, Defaults: def, Effective: shown,
			File: QoSFile(a.c.root)}
		if u, err := a.c.RunningUsage(r.Context(), ""); err == nil {
			if n, err := a.c.Store().CountActive(r.Context()); err == nil {
				v.Usage = map[string]any{
					"submitted_jobs": n, "running_jobs": u.Jobs,
					"running_cpus": u.CPUs, "running_gpus": u.GPUs, "running_mem_mb": u.MemMB,
				}
			}
		}
		writeJSON(w, http.StatusOK, v)
		return
	}

	var over qos.Limits
	if user != "" {
		var err error
		if over, err = a.c.UserQoS(r.Context(), user); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	eff := qos.Resolve(def, over)
	shown := map[string]string{}
	for _, f := range qos.Fields {
		shown[f.Name] = f.Get(eff)
	}
	v := qosView{Cluster: cfg.Cluster, Defaults: def, Overrides: over,
		Effective: shown, File: QoSFile(a.c.root)}
	if u, err := a.c.RunningUsage(r.Context(), user); err == nil {
		if n, err := a.c.Store().CountActiveForUser(r.Context(), user); err == nil {
			v.Usage = map[string]any{
				"submitted_jobs": n, "running_jobs": u.Jobs,
				"running_cpus": u.CPUs, "running_gpus": u.GPUs, "running_mem_mb": u.MemMB,
			}
		}
	}
	writeJSON(w, http.StatusOK, v)
}

// setQoSDefaults replaces the per-account default limits.
func (a *API) setQoSDefaults(w http.ResponseWriter, r *http.Request, caller *store.User) {
	var l qos.Limits
	if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
		fail(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	if err := a.c.SetQoSDefaults(l); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.c.log.Info("per-account QoS defaults changed", "admin", caller.Name)
	writeJSON(w, http.StatusOK, map[string]any{"defaults": l, "file": QoSFile(a.c.root)})
}

// setQoSCluster replaces the aggregate ceiling across everybody.
func (a *API) setQoSCluster(w http.ResponseWriter, r *http.Request, caller *store.User) {
	var l qos.Limits
	if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
		fail(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	if err := a.c.SetQoSCluster(l); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.c.log.Info("cluster-wide QoS ceiling changed", "admin", caller.Name)
	writeJSON(w, http.StatusOK, map[string]any{"cluster": l, "file": QoSFile(a.c.root)})
}

// setUserQoS replaces one account's overrides.
func (a *API) setUserQoS(w http.ResponseWriter, r *http.Request, caller *store.User) {
	name := r.PathValue("name")
	var l qos.Limits
	if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
		fail(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	if err := a.c.SetUserQoS(r.Context(), name, l); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.c.log.Info("account QoS changed", "admin", caller.Name, "user", name)
	eff, _ := a.c.EffectiveQoS(r.Context(), name)
	shown := map[string]string{}
	for _, f := range qos.Fields {
		shown[f.Name] = f.Get(eff)
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": name, "effective": shown})
}
