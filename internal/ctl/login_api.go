package ctl

import (
	"net/http"
	"sync"

	"github.com/davidwu/shome/internal/store"
)

// Where to reach the login node.
//
// Served by the controller because only the controller knows: the addresses
// come from its own configuration and interfaces, and the port from the
// socket it is actually listening on.
//
// The alternative -- and what this replaces -- was for the CLI to work it out
// locally. That only ever worked when the admin happened to be sitting at the
// controller: run `shome admin enroll` from a node, or from a login session
// where the state directory is a bridge rather than an installation, and the
// instructions came out with a literal "<controller>" in them for the person
// to guess at.

// LoginInfo is how an outside user reaches this cluster.
type LoginInfo struct {
	// Enabled is false when no login node is running, in which case an
	// enrollment code is useless and saying so beats printing instructions
	// that cannot work.
	Enabled bool `json:"enabled"`
	Port    int  `json:"port"`
	// Hosts are the addresses to try, best first. More than one when the
	// machine has several interfaces: which is reachable depends on the
	// network the other person is on, and only they know that.
	Hosts []string `json:"hosts"`

	// Reason says why there is no login node, when one was configured and
	// did not start.
	//
	// "Off" and "meant to be on and broken" are different situations and
	// call for different reactions. Without this they were the same empty
	// answer, and `shome up` printed "login node ssh on 0.0.0.0:2222" from
	// the configuration whatever had happened -- so an address something
	// else was holding was advertised as this cluster's, and people who
	// followed the instruction reached whatever was actually there.
	Reason string `json:"reason,omitempty"`
}

// loginInfo holds what the daemon told us about ourselves.
type loginState struct {
	mu   sync.Mutex
	info LoginInfo
}

// SetLoginInfo records where the login node is reachable. Called by the
// daemon at startup, which is what owns the configuration.
func (c *Controller) SetLoginInfo(info LoginInfo) {
	c.login.mu.Lock()
	c.login.info = info
	c.login.mu.Unlock()
}

// LoginInfo reports where the login node is reachable.
func (c *Controller) LoginInfo() LoginInfo {
	c.login.mu.Lock()
	defer c.login.mu.Unlock()
	return c.login.info
}

// loginWhere answers "what do I tell someone to ssh to".
//
// Readable by any account: it is the address they already connected to, and
// a user who wants to add another of their own machines needs it.
func (a *API) loginWhere(w http.ResponseWriter, r *http.Request, _ *store.User) {
	writeJSON(w, http.StatusOK, a.c.LoginInfo())
}
