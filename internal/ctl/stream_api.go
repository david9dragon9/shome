package ctl

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/store"
)

// The relay's two doors: one for the user's terminal, one for the node.
//
// Each direction is its own request because an agent's HTTP client cannot
// hold a single bidirectional connection open through every proxy it might
// sit behind, and because two unidirectional streams are far easier to reason
// about than one framed protocol. Neither request completes until the session
// ends, which is what makes this live rather than polled.

// streamOut streams a session's output to the user's terminal.
func (a *API) streamOut(w http.ResponseWriter, r *http.Request, caller *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad session id")
		return
	}
	_, out, _, err := a.c.AttachClient(id, caller.Name)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	// Flushed as it arrives. Buffering would make an interactive session
	// unusable: a prompt would not appear until enough had accumulated.
	w.WriteHeader(http.StatusOK)
	flushingCopy(w, out)
	a.c.CloseStream(id)
}

// streamIn carries the user's keystrokes into the session.
func (a *API) streamIn(w http.ResponseWriter, r *http.Request, caller *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad session id")
		return
	}
	s, err := a.c.stream(id, caller.Name)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	// Straight into the pipe the node is reading. The client's request body
	// ending means end-of-input, so the writer is closed.
	io.Copy(s.inW, r.Body)
	s.inW.Close()
	writeJSON(w, http.StatusOK, map[string]string{"status": "input closed"})
}

// streamResize tells the session how big the user's window is.
//
// Its own request rather than an escape sequence in the input stream: the
// relay does not interpret what it carries, and it must not start -- a
// resize smuggled through the keystroke channel would be indistinguishable
// from bytes the user typed.
func (a *API) streamResize(w http.ResponseWriter, r *http.Request, caller *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad session id")
		return
	}
	var ws job.Winsize
	if err := json.NewDecoder(r.Body).Decode(&ws); err != nil {
		fail(w, http.StatusBadRequest, "bad terminal size")
		return
	}
	if err := a.c.SetStreamSize(id, caller.Name, ws); err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resized"})
}

// streamStatus reports a session's exit status once it has one.
func (a *API) streamStatus(w http.ResponseWriter, r *http.Request, caller *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad session id")
		return
	}
	code, exited := a.c.StreamExit(id, caller.Name)
	writeJSON(w, http.StatusOK, map[string]any{"exit": code, "finished": exited})
}

// flushingCopy copies and flushes after every chunk.
func flushingCopy(w http.ResponseWriter, r io.Reader) {
	f, _ := w.(http.Flusher)
	buf := make([]byte, 8<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if f != nil {
				f.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// The node's side of the relay. Authorised by the node's certificate
// identity, which the agent server has already verified, plus a check that
// the job really is assigned to that node -- otherwise any machine in the
// cluster could attach to any session.

// agentStreamOut receives a session's output from the node running it.
func (s *AgentServer) agentStreamOut(w http.ResponseWriter, r *http.Request) {
	node := requirePeer(w, r)
	if node == "" {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad session id", http.StatusBadRequest)
		return
	}
	if err := s.c.streamBelongsTo(r.Context(), id, node); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	_, out, err := s.c.AttachNode(id, node)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	// The node's request body is the process's output.
	io.Copy(out, r.Body)
	// Ended: the process's terminal closed. The exit status arrives
	// separately, because it is not known until wait() returns.
	out.Close()
	w.WriteHeader(http.StatusOK)
}

// agentStreamIn sends the user's keystrokes down to the node.
func (s *AgentServer) agentStreamIn(w http.ResponseWriter, r *http.Request) {
	node := requirePeer(w, r)
	if node == "" {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad session id", http.StatusBadRequest)
		return
	}
	if err := s.c.streamBelongsTo(r.Context(), id, node); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	st, err := s.c.stream(id, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	flushingCopy(w, st.inR)
}

// agentStreamSize hands the node the user's window size, waiting until there
// is one it has not seen.
//
// Long-polled: the node asks for anything newer than the version it holds
// and the request sits open until there is one. A bounded wait rather than
// an unbounded one so that an idle session's request does not look like a
// leak to anything counting connections, and so a node that missed a change
// while reconnecting picks it up on the next round.
func (s *AgentServer) agentStreamSize(w http.ResponseWriter, r *http.Request) {
	node := requirePeer(w, r)
	if node == "" {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad session id", http.StatusBadRequest)
		return
	}
	if err := s.c.streamBelongsTo(r.Context(), id, node); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	ctx, cancel := context.WithTimeout(r.Context(), streamSizePoll)
	defer cancel()
	ws, ver, ok := s.c.WatchStreamSize(ctx, id, since)
	if !ok {
		// Nothing new. Not an error: the node asks again, and saying so
		// with a status rather than a body keeps the two cases apart.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, streamSize{Winsize: ws, Version: ver})
}

// streamSize is a window size on the wire, with the version that identifies
// it. JSON rather than the four numbers alone so a node can tell a size it
// has already applied from one it has not.
type streamSize struct {
	job.Winsize
	Version int64 `json:"version"`
}

// streamSizePoll bounds one long poll for a window size.
const streamSizePoll = 60 * time.Second

// agentStreamExit records the process's exit status and ends the session.
func (s *AgentServer) agentStreamExit(w http.ResponseWriter, r *http.Request) {
	node := requirePeer(w, r)
	if node == "" {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad session id", http.StatusBadRequest)
		return
	}
	if err := s.c.streamBelongsTo(r.Context(), id, node); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	code, _ := strconv.Atoi(r.URL.Query().Get("code"))
	s.c.FinishStream(id, code)
	w.WriteHeader(http.StatusOK)
}
