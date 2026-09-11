package ctl

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/pki"
)

// AgentServer exposes the controller to node agents over mTLS.
type AgentServer struct {
	c  *Controller
	ca *pki.CA
	// api is the ordinary user API, served to jobs through their node. See
	// jobapi.go for why a job reaches the cluster this way.
	api http.Handler
	// routeTable records what routes() registered and whether each one needs
	// a node certificate, so a test can assert over every agent endpoint
	// rather than the ones somebody remembered. See agentguard_test.go.
	routeTable []agentRoute
}

// agentRoute is one registered agent endpoint.
type agentRoute struct {
	Pattern string
	// NeedsPeer is false for exactly one route, join, which is how a machine
	// with no certificate yet gets one.
	NeedsPeer bool
}

// registeredAgentRoutes reports what routes() installed.
func (s *AgentServer) registeredAgentRoutes() []agentRoute { return s.routeTable }

// JoinTokenTTL is how long a join token stays usable. Short: it is copied by
// hand between machines and ends up in shell history.
const JoinTokenTTL = 30 * time.Minute

func (s *AgentServer) routes() *http.ServeMux {
	m := http.NewServeMux()
	s.routeTable = nil
	// reg records whether a route requires the caller to be a known node, so
	// that the claim below is checkable rather than a convention. A handler
	// still calls requirePeer itself -- it needs the peer's name, not just
	// the fact of it -- and the test asserts the two agree for every route.
	reg := func(pattern string, needsPeer bool, h http.HandlerFunc) {
		s.routeTable = append(s.routeTable, agentRoute{Pattern: pattern, NeedsPeer: needsPeer})
		m.HandleFunc(pattern, h)
	}
	// join is the one route without a certificate, because it is where a
	// machine gets one. It spends a single-use token instead.
	reg("POST /agent/join", false, s.join)
	reg("POST /agent/heartbeat", true, s.heartbeat)
	reg("POST /agent/leave", true, s.leave)
	reg("POST /agent/userfile/{id}", true, s.uploadUserFile)
	reg("GET /agent/userfile/{id}", true, s.downloadUserFile)
	reg("POST /agent/output/{id}", true, s.uploadOutput)
	reg("GET /agent/stagein/{id}", true, s.downloadStageIn)
	reg("POST /agent/stageout/{id}", true, s.uploadStageOut)
	// The node's half of an interactive session. Two unidirectional streams
	// plus a status report; see internal/ctl/stream.go for why.
	reg("POST /agent/stream/{id}/out", true, s.agentStreamOut)
	reg("GET /agent/stream/{id}/in", true, s.agentStreamIn)
	reg("GET /agent/stream/{id}/size", true, s.agentStreamSize)
	reg("POST /agent/stream/{id}/exit", true, s.agentStreamExit)
	// The cluster API, for jobs running on this node. Authorised twice: the
	// node's certificate to get here, the job's own token to be answered.
	reg("/agent/api/", true, s.jobAPI)
	return m
}

// requirePeer returns the verified node name, or "" after writing an error.
//
// Every agent endpoint except join must call this. The TLS layer is
// configured to allow certless connections, because otherwise join -- which
// is how a machine gets its certificate -- could not happen at all. So the
// listener accepting a connection proves nothing about who is on it, and an
// endpoint that skipped this check would be reachable by anyone who can
// route to the port.
//
// Which is a claim about every endpoint, so it is asserted over every
// endpoint: see agentguard_test.go.
func requirePeer(w http.ResponseWriter, r *http.Request) string {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		http.Error(w, "client certificate required", http.StatusUnauthorized)
		return ""
	}
	name := pki.PeerName(r.TLS.VerifiedChains)
	if name == "" {
		http.Error(w, "client certificate has no subject name", http.StatusUnauthorized)
		return ""
	}
	return name
}

// leave retires the calling node from the cluster.
//
// This is the agent API rather than the client API because it is the only
// channel a worker actually has. The CLI speaks to a controller over a unix
// socket on the same machine, so a departing worker cannot use it to say
// anything at all -- the first version of `shome nuke` tried, and failed with
// a confusing complaint about a missing API token when the real problem was
// that there was nothing on the other end of the socket.
//
// A node's own certificate authorises exactly one thing here: removing
// itself. The name comes from the verified chain, never from the request, so
// a node cannot retire its neighbours.
func (s *AgentServer) leave(w http.ResponseWriter, r *http.Request) {
	name := requirePeer(w, r)
	if name == "" {
		return
	}
	if err := s.c.RemoveNode(r.Context(), name); err != nil {
		s.c.log.Warn("node could not leave", "node", name, "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.c.log.Info("node left the cluster", "node", name)
	w.WriteHeader(http.StatusOK)
}

func (s *AgentServer) join(w http.ResponseWriter, r *http.Request) {
	var req agentapi.JoinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Node == "" {
		http.Error(w, "node name is required", http.StatusBadRequest)
		return
	}
	if err := s.c.Store().RedeemJoinToken(r.Context(), req.Token, req.Node, time.Now()); err != nil {
		// Deliberately vague to the caller, specific in the log: a probing
		// client should not learn whether a token exists but is expired.
		s.c.log.Warn("join rejected", "node", req.Node, "err", err, "remote", r.RemoteAddr)
		http.Error(w, "join refused", http.StatusForbidden)
		return
	}
	certPEM, keyPEM, err := s.ca.Issue(req.Node, nil, true)
	if err != nil {
		http.Error(w, "could not issue certificate", http.StatusInternalServerError)
		return
	}
	// Joining is how a machine that was removed comes back: somebody issued
	// it a token, which is the decision to have it again.
	if err := s.c.Store().UnforgetNode(r.Context(), req.Node); err != nil {
		s.c.log.Warn("could not clear an earlier removal", "node", req.Node, "err", err)
	}
	s.c.log.Info("node joined", "node", req.Node, "remote", r.RemoteAddr)
	s.c.Store().Event(r.Context(), 0, "node_join", req.Node, time.Now())
	writeJSON(w, http.StatusOK, agentapi.JoinResponse{
		CACert: string(s.ca.CertPEM()), NodeCert: string(certPEM), NodeKey: string(keyPEM),
	})
}

func (s *AgentServer) heartbeat(w http.ResponseWriter, r *http.Request) {
	peer := requirePeer(w, r)
	if peer == "" {
		return
	}
	var hb agentapi.Heartbeat
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// A node may only speak for itself: the certificate name wins over the
	// body, so a compromised agent cannot report on or steal another's jobs.
	if hb.Node != "" && hb.Node != peer {
		s.c.log.Warn("heartbeat identity mismatch", "cert", peer, "claimed", hb.Node)
		http.Error(w, "node name does not match client certificate", http.StatusForbidden)
		return
	}
	hb.Node = peer

	resp, acked, err := s.c.HandleHeartbeat(r.Context(), hb)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp.Acked = acked
	writeJSON(w, http.StatusOK, resp)
}

// maxOutputBytes caps a single job's stored output. A runaway job can print
// without limit, and the controller's disk is the owner's disk.
const maxOutputBytes = 32 << 20

func (s *AgentServer) uploadOutput(w http.ResponseWriter, r *http.Request) {
	peer := requirePeer(w, r)
	if peer == "" {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad job id", http.StatusBadRequest)
		return
	}
	// Only a node that actually ran (a rank of) the job may upload output.
	rank, ok := s.c.AuthorizeNodeForJob(r.Context(), id, peer)
	if !ok {
		s.c.log.Warn("output upload from a node that did not run the job", "job", id, "from", peer)
		http.Error(w, "this node did not run that job", http.StatusForbidden)
		return
	}
	if err := s.c.SaveOutputRank(id, rank, io.LimitReader(r.Body, maxOutputBytes)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// jobForPeer resolves a job id and confirms the calling node owns it.
func (s *AgentServer) jobForPeer(w http.ResponseWriter, r *http.Request) (int64, bool) {
	peer := requirePeer(w, r)
	if peer == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad job id", http.StatusBadRequest)
		return 0, false
	}
	if _, ok := s.c.AuthorizeNodeForJob(r.Context(), id, peer); !ok {
		s.c.log.Warn("stage access from a node not running the job", "job", id, "from", peer)
		http.Error(w, "this node is not running that job", http.StatusForbidden)
		return 0, false
	}
	return id, true
}

func (s *AgentServer) downloadStageIn(w http.ResponseWriter, r *http.Request) {
	id, ok := s.jobForPeer(w, r)
	if !ok {
		return
	}
	f, err := os.Open(s.c.StageInPath(id))
	if err != nil {
		http.Error(w, "no stage-in archive for this job", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/gzip")
	io.Copy(w, f)
}

func (s *AgentServer) uploadStageOut(w http.ResponseWriter, r *http.Request) {
	id, ok := s.jobForPeer(w, r)
	if !ok {
		return
	}
	if err := s.c.SaveStage(s.c.StageOutPath(id), io.LimitReader(r.Body, maxStageBytes)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// maxStageBytes caps a results upload; the controller's disk is the owner's.
const maxStageBytes = 2 << 30

// ServeAgents starts the mTLS listener for node agents.
func ServeAgents(c *Controller, ca *pki.CA, addr string, hosts []string) (*http.Server, net.Listener, error) {
	certPEM, keyPEM, err := ca.Issue("shome-controller", hosts, false)
	if err != nil {
		return nil, nil, err
	}
	tlsCfg, err := ca.ServerTLSJoinable(certPEM, keyPEM)
	if err != nil {
		return nil, nil, err
	}
	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		return nil, nil, err
	}
	s := &AgentServer{c: c, ca: ca, api: (&API{c: c}).routes()}
	return &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}, ln, nil
}

// AgentStatePath is where an agent keeps its issued identity.
func AgentStatePath(root string) string { return filepath.Join(root, "agent") }

// SaveAgentIdentity persists the certificate material an agent received.
func SaveAgentIdentity(dir string, jr agentapi.JoinResponse) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for name, content := range map[string]string{
		"ca.crt": jr.CACert, "node.crt": jr.NodeCert, "node.key": jr.NodeKey,
	} {
		mode := os.FileMode(0o644)
		if name == "node.key" {
			mode = 0o600
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
			return fmt.Errorf("save %s: %w", name, err)
		}
	}
	return nil
}

// LoadAgentIdentity reads previously saved certificate material.
func LoadAgentIdentity(dir string) (ca, cert, key []byte, err error) {
	if ca, err = os.ReadFile(filepath.Join(dir, "ca.crt")); err != nil {
		return nil, nil, nil, err
	}
	if cert, err = os.ReadFile(filepath.Join(dir, "node.crt")); err != nil {
		return nil, nil, nil, err
	}
	if key, err = os.ReadFile(filepath.Join(dir, "node.key")); err != nil {
		return nil, nil, nil, err
	}
	return ca, cert, key, nil
}
