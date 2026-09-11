package loginnode

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/davidwu/shome/internal/store"
)

// Authentication maps an SSH connection to a shome account.
//
// Two ways in, both resolving to the same place -- the accounts an admin
// created:
//
//  1. A public key the admin registered against the account. This is the
//     ordinary path: someone sends you their id_ed25519.pub, you authorise it,
//     they ssh in. No certificate machinery for them to understand.
//  2. A certificate signed by the cluster's user CA, as `shome login` issues.
//     Short-lived, so nothing durable sits on a laptop, and revocable by
//     letting it expire.
//
// In both cases the account must exist and not be suspended at the moment of
// connection -- so `shome admin quarantine` locks somebody out of SSH
// immediately, rather than at the next certificate renewal.

// authCtxKey identifies the resolved account on a connection.
const authUserKey = "shome-user"

func (s *Server) authPublicKey(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	requested := conn.User()
	if cert, ok := key.(*ssh.Certificate); ok {
		return s.authCertificate(ctx, conn, cert, requested)
	}

	fp := ssh.FingerprintSHA256(key)
	u, err := s.cfg.Store.UserByKeyFingerprint(ctx, fp)
	if err != nil {
		// Unknown key. Remember it against this connection: if the client
		// falls through to enrollment and presents a valid code, this is the
		// key that gets registered. An ssh client offers its public keys
		// before trying anything interactive, so by the time a code arrives
		// the key is already here -- which is what lets somebody enroll with
		// nothing but a stock ssh client and a code.
		s.rememberOffered(conn, key)
		return nil, fmt.Errorf("no account authorises this key")
	}
	// The SSH username must match the account the key belongs to. Allowing a
	// mismatch would let one person log in under another's name while
	// authenticating as themselves, which makes every audit entry a lie.
	if requested != u.Name {
		return nil, fmt.Errorf("this key belongs to a different account")
	}
	s.cfg.Store.TouchUserKey(ctx, fp, time.Now())
	s.cfg.Log.Info("login accepted", "user", u.Name, "method", "key",
		"fingerprint", fp, "remote", conn.RemoteAddr().String())
	return permissionsFor(u, fp), nil
}

// authCertificate validates a certificate issued by this cluster's user CA.
func (s *Server) authCertificate(ctx context.Context, conn ssh.ConnMetadata,
	cert *ssh.Certificate, requested string) (*ssh.Permissions, error) {

	if s.cfg.UserCA == nil {
		return nil, fmt.Errorf("this cluster does not accept certificates")
	}
	if cert.CertType != ssh.UserCert {
		return nil, fmt.Errorf("not a user certificate")
	}
	caKey, _, _, _, err := ssh.ParseAuthorizedKey(s.cfg.UserCA.PublicKey())
	if err != nil {
		return nil, fmt.Errorf("cluster CA is unreadable")
	}
	// Compare marshalled bytes: a certificate signed by some other CA must not
	// be accepted just because it names a principal we recognise.
	if string(cert.SignatureKey.Marshal()) != string(caKey.Marshal()) {
		return nil, fmt.Errorf("certificate was not issued by this cluster")
	}

	checker := &ssh.CertChecker{
		IsUserAuthority: func(k ssh.PublicKey) bool {
			return string(k.Marshal()) == string(caKey.Marshal())
		},
	}
	// CheckCert enforces validity dates, critical options and the principal
	// list. Doing those by hand is how certificate authentication ends up
	// accepting expired credentials.
	if err := checker.CheckCert(requested, cert); err != nil {
		return nil, fmt.Errorf("certificate rejected: %w", err)
	}

	// The certificate says who they are; the account table says whether they
	// still exist and are still allowed.
	u, err := s.cfg.Store.UserByName(ctx, requested)
	switch {
	case errors.Is(err, store.ErrAccountDisabled):
		// Separated from a missing account because the two need different
		// things done about them, and this message is what an admin reads in
		// the log when somebody reports being unable to log in.
		return nil, fmt.Errorf("account is suspended")
	case err != nil:
		return nil, fmt.Errorf("no such account")
	}
	s.cfg.Log.Info("login accepted", "user", u.Name, "method", "certificate",
		"serial", cert.Serial, "fingerprint", ssh.FingerprintSHA256(cert.Key),
		"remote", conn.RemoteAddr().String())
	// No key fingerprint, deliberately. The field means "the registered key
	// this session signed in with", and a certificate is not one: it is a
	// separate credential, bounded by its own expiry, issued by `shome
	// login` to a key nobody enrolled.
	//
	// Recording the certificate's key here instead made every certificate
	// session unusable. The authorisation re-check looks the fingerprint up
	// among the account's registered keys, found nothing -- because there
	// was nothing to find -- and concluded the computer had been signed
	// out, so `shome login` issued a certificate that was refused by the
	// first command after connecting.
	//
	// Everything downstream already means the right thing when it is empty:
	// the re-check falls back to the account alone, `shome unenroll` says
	// there is no single computer to sign out of, and `unenroll --all`
	// still closes the session because it matches every key.
	return permissionsFor(u, ""), nil
}

// permissionsFor records the resolved identity on the connection and refuses
// every SSH feature that could widen it.
func permissionsFor(u *store.User, fingerprint string) *ssh.Permissions {
	return &ssh.Permissions{
		Extensions: map[string]string{
			authUserKey:   u.Name,
			"shome-role":  string(u.Role),
			"shome-keyfp": fingerprint,
		},
		// No permit-* options are granted. x/crypto/ssh does not act on these
		// itself -- the channel and request handling is what actually refuses
		// forwarding and PTYs -- but recording the intent here keeps the
		// policy visible next to the identity it applies to.
		CriticalOptions: map[string]string{},
	}
}

// keyFingerprintOf returns the key this connection authenticated with.
func keyFingerprintOf(perms *ssh.Permissions) string {
	if perms == nil || perms.Extensions == nil {
		return ""
	}
	return perms.Extensions["shome-keyfp"]
}

// userOf returns the account resolved during authentication.
func userOf(perms *ssh.Permissions) string {
	if perms == nil || perms.Extensions == nil {
		return ""
	}
	return perms.Extensions[authUserKey]
}
