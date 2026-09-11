package ctl

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/davidwu/shome/internal/store"
)

// Managing which public keys may log in.
//
// Admin-only, because this is the cluster's admission list: authorising a key
// is exactly as consequential as creating the account it belongs to.

type keyRequest struct {
	User      string `json:"user"`
	PublicKey string `json:"public_key"`
}

func (a *API) addUserKey(w http.ResponseWriter, r *http.Request, caller *store.User) {
	var req keyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	req.User = strings.TrimSpace(req.User)
	req.PublicKey = strings.TrimSpace(req.PublicKey)
	if req.User == "" || req.PublicKey == "" {
		fail(w, http.StatusBadRequest, "user and public_key are required")
		return
	}

	// Parse before storing. An unparseable key would be accepted here and
	// then never match anything at login, leaving an admin certain they had
	// authorised someone who cannot get in.
	pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(req.PublicKey))
	if err != nil {
		fail(w, http.StatusBadRequest,
			"that does not look like an SSH public key: "+err.Error()+
				"\nExpecting the contents of a .pub file, e.g. 'ssh-ed25519 AAAA... you@laptop'")
		return
	}
	// Refuse a certificate: certificates expire and are issued by the CA, so
	// registering one as a standing authorisation would outlive its validity
	// and confuse the two mechanisms.
	if _, isCert := pub.(*ssh.Certificate); isCert {
		fail(w, http.StatusBadRequest,
			"that is a certificate, not a public key; register the .pub key itself")
		return
	}

	fp := ssh.FingerprintSHA256(pub)
	// Store the normalised form, not what was pasted, so whitespace and
	// options cannot smuggle anything into the file we later display.
	normalised := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	if err := a.c.Store().AddUserKey(r.Context(), req.User, fp, normalised, comment, time.Now()); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.c.log.Info("ssh key authorised", "admin", caller.Name, "user", req.User, "fingerprint", fp)
	writeJSON(w, http.StatusOK, map[string]string{
		"user": req.User, "fingerprint": fp, "comment": comment,
	})
}

// listUserKeys reports every way into an account: the machines that can log
// in, and the enrollment codes that could still become one.
//
// Both, because they are the same question. A listing showing only registered
// keys answers "who can get in right now" while leaving out "and who could let
// themselves in this afternoon", which is exactly what somebody auditing
// access needs to see.
func (a *API) listUserKeys(w http.ResponseWriter, r *http.Request, caller *store.User) {
	user := r.URL.Query().Get("user")
	// A plain user may see their own access and nobody else's: another
	// person's fingerprints are an inventory of their computers.
	if caller.Role == store.RoleUser {
		user = caller.Name
	}
	keys, err := a.c.Store().UserKeys(r.Context(), user)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	codes, err := a.c.Store().PendingCodes(r.Context(), user, time.Now())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}

	type kv struct {
		User        string `json:"user"`
		Fingerprint string `json:"fingerprint"`
		Comment     string `json:"comment"`
		Added       string `json:"added"`
		LastUsed    string `json:"last_used,omitempty"`
	}
	type cv struct {
		User    string `json:"user"`
		ID      string `json:"id"`
		Issued  string `json:"issued"`
		Expires string `json:"expires"`
	}
	outKeys := make([]kv, 0, len(keys))
	for _, k := range keys {
		v := kv{User: k.User, Fingerprint: k.Fingerprint, Comment: k.Comment,
			Added: k.CreatedAt.Format(time.RFC3339)}
		if !k.LastUsed.IsZero() {
			v.LastUsed = k.LastUsed.Format(time.RFC3339)
		}
		outKeys = append(outKeys, v)
	}
	outCodes := make([]cv, 0, len(codes))
	for _, c := range codes {
		outCodes = append(outCodes, cv{User: c.User, ID: c.ID,
			Issued:  c.CreatedAt.Format(time.RFC3339),
			Expires: c.ExpiresAt.Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": outKeys, "codes": outCodes})
}

// invalidateCode cancels one outstanding enrollment code without touching any
// machine that is already enrolled.
func (a *API) invalidateCode(w http.ResponseWriter, r *http.Request, caller *store.User) {
	handle := r.PathValue("id")
	user, err := a.c.Store().InvalidateCodeByHandle(r.Context(), handle, time.Now())
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.c.log.Info("enrollment code invalidated", "admin", caller.Name, "user", user, "code", handle)
	writeJSON(w, http.StatusOK, map[string]string{"user": user, "code": handle})
}

// delUserKey revokes one key by fingerprint alone, looking up its owner.
//
// Routed through the same Unenroll path as everything else, so revoking a key
// has one meaning regardless of which command reached it -- including spending
// the owner's outstanding codes, which this used not to do.
func (a *API) delUserKey(w http.ResponseWriter, r *http.Request, caller *store.User) {
	fp := r.PathValue("fingerprint")
	owner, err := a.c.OwnerOfKey(r.Context(), fp)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := a.c.Unenroll(r.Context(), owner, fp, false)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.c.log.Info("ssh key revoked", "admin", caller.Name, "user", owner, "fingerprint", fp)
	writeJSON(w, http.StatusOK, res)
}

// enrollCode mints a single-use code that lets someone register their own key
// on first connection, so onboarding needs no out-of-band key exchange.
func (a *API) enrollCode(w http.ResponseWriter, r *http.Request, caller *store.User) {
	user := r.PathValue("name")
	code, err := a.c.Store().CreateEnrollCode(r.Context(), user, time.Now())
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.c.log.Info("enrollment code issued", "admin", caller.Name, "user", user)
	writeJSON(w, http.StatusOK, map[string]string{
		"user": user, "code": code, "expires_in": store.EnrollTTL.String(),
	})
}

// enrollSelf mints a code for the caller's own account.
//
// A person routinely has more than one computer, and needing an admin for each
// of them makes the cluster feel administered rather than used. Someone
// already authenticated as themselves can already register a key by any means
// available to them, so letting them issue a code for their own account grants
// nothing new -- it just removes a person from the loop.
func (a *API) enrollSelf(w http.ResponseWriter, r *http.Request, caller *store.User) {
	code, err := a.c.Store().CreateEnrollCode(r.Context(), caller.Name, time.Now())
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.c.log.Info("self-service enrollment code issued", "user", caller.Name)
	writeJSON(w, http.StatusOK, map[string]string{
		"user": caller.Name, "code": code, "expires_in": store.EnrollTTL.String(),
	})
}

// unenroll is signing out: it removes a key the caller logs in with, and
// invalidates every outstanding enrollment code for their account.
//
// The case it exists for is a public or borrowed computer. Leaving a key
// registered there means whoever sits down next has the person's cluster
// access, and leaving an unspent code alive means they could re-enroll with it.
// So both go, and they go together -- a sign-out that left either behind would
// be worse than none, because it would look like it had worked.
//
// The account itself is untouched: jobs, storage, quota and role all survive,
// and the person can enroll again from any machine they still have access on.
func (a *API) unenroll(w http.ResponseWriter, r *http.Request, caller *store.User) {
	var req struct {
		Fingerprint string `json:"fingerprint"`
		All         bool   `json:"all"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	res, err := a.c.Unenroll(r.Context(), caller.Name, req.Fingerprint, req.All)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// adminUnenroll signs out a machine on somebody else's behalf: a lost laptop,
// a departing colleague, a shared computer nobody should still be reaching the
// cluster from.
func (a *API) adminUnenroll(w http.ResponseWriter, r *http.Request, caller *store.User) {
	var req struct {
		Fingerprint string `json:"fingerprint"`
		All         bool   `json:"all"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	user := r.PathValue("name")
	res, err := a.c.Unenroll(r.Context(), user, req.Fingerprint, req.All)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.c.log.Info("admin signed out a machine", "admin", caller.Name, "user", user,
		"keys", len(res.Removed))
	writeJSON(w, http.StatusOK, res)
}
