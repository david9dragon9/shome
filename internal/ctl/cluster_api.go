package ctl

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/davidwu/shome/internal/clustercfg"
	"github.com/davidwu/shome/internal/store"
)

// The cluster's own identity: its name, and what the admin calls each
// machine. Read by anyone (a shell prompt needs it), written by an admin.

// ClusterView is what the cluster is called, and what its machines are called.
type ClusterView struct {
	Name  string            `json:"name"`
	Nodes []ClusterNodeName `json:"nodes"`
}

// ClusterNodeName pairs a machine's certificate identity with its display
// name. Both are reported because they can differ, and an admin debugging a
// certificate problem needs the one underneath.
type ClusterNodeName struct {
	Identity string `json:"identity"`
	Label    string `json:"label"`
	Note     string `json:"note,omitempty"`
}

// clusterInfo reports the cluster's name and its machines' names.
//
// Readable by any account, not just admins: a login shell puts the cluster
// name in its prompt, and a name is not a secret.
func (a *API) clusterInfo(w http.ResponseWriter, r *http.Request, _ *store.User) {
	cfg := a.c.ClusterConfig()
	out := ClusterView{Name: cfg.ClusterName()}
	nodes, err := a.c.Store().Nodes(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, ClusterNodeName{
			Identity: n.Name, Label: cfg.LabelOf(n.Name), Note: cfg.NoteOf(n.Name),
		})
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].Label < out.Nodes[j].Label })
	writeJSON(w, http.StatusOK, out)
}

// setClusterName renames the cluster.
func (a *API) setClusterName(w http.ResponseWriter, r *http.Request, caller *store.User) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	cfg := a.c.ClusterConfig()
	if req.Name == "" {
		cfg.Name = ""
	} else {
		if err := clustercfg.ValidName(req.Name); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		cfg.Name = req.Name
	}
	if err := a.c.SetClusterConfig(cfg); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.c.log.Info("cluster renamed", "admin", caller.Name, "name", cfg.ClusterName())
	writeJSON(w, http.StatusOK, map[string]string{"name": cfg.ClusterName()})
}

// setNodeName labels a machine, or clears its label.
func (a *API) setNodeName(w http.ResponseWriter, r *http.Request, caller *store.User) {
	var req struct {
		Node  string  `json:"node"`
		Label *string `json:"label,omitempty"`
		Note  *string `json:"note,omitempty"`
		Clear bool    `json:"clear,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	// Resolved, so an admin can rename a machine using the name they gave it
	// last time rather than having to remember its certificate identity.
	id, err := a.c.NodeIdentity(r.Context(), req.Node)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg := a.c.ClusterConfig()
	if cfg.Nodes == nil {
		cfg.Nodes = map[string]clustercfg.Node{}
	}
	entry := cfg.Nodes[id]
	switch {
	case req.Clear:
		// Clearing the label leaves any note alone: they are separate things
		// and clearing one to change the other would be a surprise.
		if req.Note != nil {
			entry.Note = ""
		} else {
			entry.Label = ""
		}
	case req.Label != nil && *req.Label != "":
		var identities []string
		if nodes, err := a.c.Store().Nodes(r.Context()); err == nil {
			for _, n := range nodes {
				identities = append(identities, n.Name)
			}
		}
		name := clustercfg.NormaliseLabel(id, *req.Label)
		if name == "" {
			// They asked for the machine's own identity, which is what it is
			// called with no label at all.
			entry.Label = ""
			break
		}
		if err := cfg.CanLabel(id, name, identities); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		entry.Label = name
	case req.Note != nil:
		entry.Note = strings.TrimSpace(*req.Note)
	default:
		fail(w, http.StatusBadRequest, "nothing to set")
		return
	}
	cfg.Nodes[id] = entry
	if err := a.c.SetClusterConfig(cfg); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.c.log.Info("machine renamed", "admin", caller.Name,
		"identity", id, "label", entry.Label)
	writeJSON(w, http.StatusOK, map[string]string{
		"node": id, "label": a.c.ClusterConfig().LabelOf(id), "note": entry.Note,
	})
}
