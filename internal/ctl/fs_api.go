package ctl

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/store"
)

// The user-facing filesystem API.
//
// Reads are served from the controller's index, which is a cache: it is
// whatever the machines last reported, and it says so. That is a deliberate
// trade -- listing a directory on a sleeping laptop would otherwise be
// impossible, and waiting for a round trip to every machine would make `ls`
// as slow as the slowest node.
//
// Writes are actions queued to the machine that owns the file, because only
// it can carry them out.

// FSEntry is one file as the user sees it.
type FSEntry struct {
	Node  string `json:"node"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	MTime string `json:"mtime"`
}

// FSUsage is one account's storage, per machine and in total.
// FSUsage is what an account occupies, per machine and in total.
//
// In bytes rather than whole megabytes: rounding here would report a small
// file as nothing at all, and the total is the figure a cluster-wide limit is
// enforced against. Rounding is the display layer's job.
type FSUsage struct {
	User       string        `json:"user"`
	TotalBytes int64         `json:"total_bytes"`
	LimitBytes int64         `json:"limit_bytes"` // -1 when unlimited
	Files      int           `json:"files"`
	Nodes      []FSNodeUsage `json:"nodes"`
	Truncated  bool          `json:"truncated,omitempty"`
}

// FSNodeUsage is one account's storage on one machine.
type FSNodeUsage struct {
	Node  string `json:"node"`
	Bytes int64  `json:"bytes"`
	Files int    `json:"files"`
	AsOf  string `json:"as_of"`
	Stale bool   `json:"stale"`
}

// named reports a machine by the name the admin gave it.
//
// Applied here, at the edge, rather than in the store: identities are what
// the database, the certificates and the job history are keyed on, and
// rewriting them internally to suit a display name is how a rename ends up
// corrupting records. Everything a person reads passes through this; the
// names it produces are also accepted back on input, so they round-trip.
func (a *API) named(identity string) string { return a.c.LabelOf(identity) }

// labelled copies a transfer with its machines named for display.
func (a *API) labelled(t *Transfer) Transfer {
	out := *t
	// The relay endpoints for the user's own computer are placeholder text
	// rather than machines, so LabelOf passes them through untouched.
	out.FromNode = a.named(t.FromNode)
	out.ToNode = a.named(t.ToNode)
	return out
}

func (a *API) fsList(w http.ResponseWriter, r *http.Request, caller *store.User) {
	node := r.URL.Query().Get("node")
	if node != "" {
		id, err := a.c.NodeIdentity(r.Context(), node)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		node = id
	}
	prefix := strings.TrimPrefix(r.URL.Query().Get("path"), "/")
	files, err := a.c.Store().UserFiles(r.Context(), caller.Name, node, prefix, 5000)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]FSEntry, 0, len(files))
	for _, f := range files {
		out = append(out, FSEntry{Node: a.named(f.Node), Path: f.Path, Size: f.Size,
			MTime: f.MTime.Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) fsUsage(w http.ResponseWriter, r *http.Request, caller *store.User) {
	// An admin may ask about somebody else; anyone else gets themselves.
	user := caller.Name
	if q := r.URL.Query().Get("user"); q != "" && caller.Role == store.RoleAdmin {
		user = q
	}
	writeJSON(w, http.StatusOK, a.usageFor(r, user))
}

func (a *API) usageFor(r *http.Request, user string) FSUsage {
	out := FSUsage{User: user, LimitBytes: -1}
	if e, err := a.c.EffectiveQoS(r.Context(), user); err == nil && e.MaxDiskMB >= 0 {
		out.LimitBytes = e.MaxDiskMB << 20
	}
	per, err := a.c.Store().UserDiskByNode(r.Context(), user)
	if err != nil {
		return out
	}
	now := time.Now()
	for _, u := range per {
		out.TotalBytes += u.Bytes
		out.Files += u.Files
		if u.Truncated {
			out.Truncated = true
		}
		out.Nodes = append(out.Nodes, FSNodeUsage{
			Node: a.named(u.Node), Bytes: u.Bytes, Files: u.Files,
			AsOf: u.At.Format(time.RFC3339),
			// A figure from a machine that has not checked in recently is
			// still counted -- its files have not stopped existing -- but it
			// is marked, so a total that looks wrong can be explained.
			Stale: now.Sub(u.At) > 2*time.Minute,
		})
	}
	return out
}

// fsRemove queues a delete on the machine that holds the file.
func (a *API) fsRemove(w http.ResponseWriter, r *http.Request, caller *store.User) {
	var req struct {
		Node      string `json:"node"`
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	if req.Node == "" || req.Path == "" {
		fail(w, http.StatusBadRequest, "node and path are required")
		return
	}
	node, err := a.c.NodeIdentity(r.Context(), req.Node)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Node = node
	// The path is scoped to the caller's own directory by the agent, which
	// resolves it under <users>/<caller>. Nothing here can name another
	// account's file, because the account is taken from the token.
	id := a.c.QueueFileRemove(caller.Name, req.Node, req.Path, req.Recursive)
	writeJSON(w, http.StatusOK, map[string]any{
		"queued": true, "transfer": id, "node": req.Node, "path": req.Path,
	})
}

func (a *API) fsTransfer(w http.ResponseWriter, r *http.Request, caller *store.User) {
	var req struct {
		FromNode string `json:"from_node"`
		FromPath string `json:"from_path"`
		ToNode   string `json:"to_node"`
		ToPath   string `json:"to_path"`
		Move     bool   `json:"move"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	t, err := a.c.StartTransfer(r.Context(), caller.Name,
		req.FromNode, req.FromPath, req.ToNode, req.ToPath, req.Move)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.labelled(t))
}

func (a *API) fsTransferStatus(w http.ResponseWriter, r *http.Request, caller *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad transfer id")
		return
	}
	t, err := a.c.TransferStatus(id, caller.Name)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.labelled(t))
}

// --- the agent side of a relay -----------------------------------------

func (s *AgentServer) uploadUserFile(w http.ResponseWriter, r *http.Request) {
	node := requirePeer(w, r)
	if node == "" {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad transfer id", http.StatusBadRequest)
		return
	}
	if err := s.c.StageTransfer(id, node, io.LimitReader(r.Body, maxUserFileBytes)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *AgentServer) downloadUserFile(w http.ResponseWriter, r *http.Request) {
	node := requirePeer(w, r)
	if node == "" {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad transfer id", http.StatusBadRequest)
		return
	}
	rc, size, err := s.c.OpenTransfer(id, node)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	io.Copy(w, rc)
}

// maxUserFileBytes bounds one relayed file.
//
// A ceiling on the relay itself, separate from any account's storage limit: a
// single file larger than this would sit on the controller's disk in one
// piece, which is a different resource from the destination's allowance.
const maxUserFileBytes = 32 << 30

// QueueFileRemove asks the machine holding a file to delete it.
func (c *Controller) QueueFileRemove(user, node, path string, recursive bool) int64 {
	c.xfer.mu.Lock()
	c.xfer.next++
	id := c.xfer.next
	c.xfer.m[id] = &Transfer{
		ID: id, User: user, FromNode: node, FromPath: path,
		State: TransferSending, At: c.now(),
	}
	c.xfer.mu.Unlock()
	c.enqueue(node, agentapi.Action{
		Kind: agentapi.ActionFileRemove, Transfer: id,
		FileUser: user, FilePath: path, Recursive: recursive,
	})
	go c.expireTransfer(id)
	return id
}

// fsUpload takes a file from the user's own computer into the cluster.
func (a *API) fsUpload(w http.ResponseWriter, r *http.Request, caller *store.User) {
	node := r.URL.Query().Get("node")
	path := strings.TrimPrefix(r.URL.Query().Get("path"), "/")
	if node == "" || path == "" {
		fail(w, http.StatusBadRequest, "node and path are required")
		return
	}
	t, err := a.c.StageUpload(r.Context(), caller.Name, node, path, r.Body)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.labelled(t))
}

// fsFetch asks a machine for a file so the caller can download it.
func (a *API) fsFetch(w http.ResponseWriter, r *http.Request, caller *store.User) {
	var req struct {
		Node string `json:"node"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	req.Path = strings.TrimPrefix(req.Path, "/")
	if req.Node == "" || req.Path == "" {
		fail(w, http.StatusBadRequest, "node and path are required")
		return
	}
	t, err := a.c.StartFetch(r.Context(), caller.Name, req.Node, req.Path)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.labelled(t))
}

// fsFetchBody streams a fetched file once the machine has sent it.
func (a *API) fsFetchBody(w http.ResponseWriter, r *http.Request, caller *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad transfer id")
		return
	}
	rc, size, err := a.c.OpenFetched(id, caller.Name)
	if err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if _, err := io.Copy(w, rc); err == nil {
		// Only once it is actually delivered: dropping the relay copy on
		// request rather than on success would lose the file to a broken
		// connection halfway through.
		a.c.FinishFetch(id)
	}
}
