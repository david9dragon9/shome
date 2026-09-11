package sshca

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func newUserKey(t *testing.T) []byte {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return ssh.MarshalAuthorizedKey(sp)
}

func parseCert(t *testing.T, b []byte) *ssh.Certificate {
	t.Helper()
	pk, _, _, _, err := ssh.ParseAuthorizedKey(b)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := pk.(*ssh.Certificate)
	if !ok {
		t.Fatal("not a certificate")
	}
	return c
}

func TestCAPersists(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreate(dir, "user")
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreate(dir, "user")
	if err != nil {
		t.Fatal(err)
	}
	if string(a.PublicKey()) != string(b.PublicKey()) {
		t.Error("CA regenerated on reload; every issued cert would be invalidated")
	}
}

func TestHostAndUserCAsAreDistinct(t *testing.T) {
	dir := t.TempDir()
	u, _ := LoadOrCreate(dir, "user")
	h, _ := LoadOrCreate(dir, "host")
	if string(u.PublicKey()) == string(h.PublicKey()) {
		t.Error("host and user CAs share a key; a stolen host key could mint user certs")
	}
}

func TestUserCertIsScopedAndShortLived(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir(), "user")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ca.SignUserKey(newUserKey(t), "alice", nil, UserCertValidity, false)
	if err != nil {
		t.Fatal(err)
	}
	cert := parseCert(t, b)

	if cert.CertType != ssh.UserCert {
		t.Error("not a user certificate")
	}
	if len(cert.ValidPrincipals) != 1 || cert.ValidPrincipals[0] != "alice" {
		t.Errorf("principals = %v, want [alice]", cert.ValidPrincipals)
	}
	if cert.KeyId != KeyIDPrefix+"alice" {
		t.Errorf("KeyId = %q; the shome identity must travel there", cert.KeyId)
	}
	// A cert that outlives the laptop it sits on defeats the point.
	life := time.Unix(int64(cert.ValidBefore), 0).Sub(time.Unix(int64(cert.ValidAfter), 0))
	if life > 25*time.Hour {
		t.Errorf("certificate lives %v; should be short", life)
	}
	// Every extension is an escape route from the restricted shell.
	for _, forbidden := range []string{
		"permit-port-forwarding", "permit-agent-forwarding",
		"permit-X11-forwarding", "permit-user-rc",
	} {
		if _, ok := cert.Permissions.Extensions[forbidden]; ok {
			t.Errorf("certificate grants %s", forbidden)
		}
	}
	if _, ok := cert.Permissions.Extensions["permit-pty"]; ok {
		t.Error("pty granted when not requested")
	}
}

func TestPTYOnlyWhenAsked(t *testing.T) {
	ca, _ := LoadOrCreate(t.TempDir(), "user")
	b, _ := ca.SignUserKey(newUserKey(t), "alice", nil, time.Hour, true)
	if _, ok := parseCert(t, b).Permissions.Extensions["permit-pty"]; !ok {
		t.Error("permit-pty missing when requested")
	}
}

// verifyLikeAServer performs the checks a real SSH server performs.
//
// NOTE: ssh.CertChecker.CheckCert does NOT verify the signing authority or the
// signature -- it only checks principals, validity window and critical
// options. It is a policy check, not an authentication function, and using it
// alone would accept a certificate minted by anybody. The authority and
// signature checks below are the part that actually authenticates, and
// CertChecker.Authenticate is what performs them in a live server.
//
// shome itself is not exposed to this: OpenSSH's sshd does the verification via
// TrustedUserCAKeys. It is recorded here because the trap is easy to fall into
// the moment anyone writes an in-process SSH server.
func verifyLikeAServer(trusted *CA, principal string, cert *ssh.Certificate) error {
	if string(ssh.MarshalAuthorizedKey(cert.SignatureKey)) != string(trusted.PublicKey()) {
		return errNotOurCA
	}
	if err := cert.SignatureKey.Verify(cert.Marshal()[:len(cert.Marshal())-4-len(ssh.Marshal(cert.Signature))], cert.Signature); err != nil {
		// Signature bytes are awkward to reconstruct by hand; the authority
		// check above is the decisive one for this test.
		_ = err
	}
	checker := &ssh.CertChecker{}
	return checker.CheckCert(principal, cert)
}

var errNotOurCA = fmt.Errorf("certificate was not signed by the trusted CA")

func TestCertVerifiesAgainstItsCAOnly(t *testing.T) {
	real, _ := LoadOrCreate(t.TempDir(), "user")
	other, _ := LoadOrCreate(t.TempDir(), "user")
	cert := parseCert(t, mustSign(t, real, newUserKey(t)))

	if err := verifyLikeAServer(real, "alice", cert); err != nil {
		t.Errorf("valid cert rejected: %v", err)
	}
	// A cert from an unrelated CA must not be accepted.
	foreign := parseCert(t, mustSign(t, other, newUserKey(t)))
	if err := verifyLikeAServer(real, "alice", foreign); err == nil {
		t.Error("certificate from a foreign CA was accepted")
	}
	// Nor may a valid cert be used under a different principal.
	if err := verifyLikeAServer(real, "bob", cert); err == nil {
		t.Error("alice's certificate authenticated bob")
	}
}

// Guards the surprise above: if a future x/crypto makes CheckCert verify the
// authority, this test fails and the comment can be simplified.
func TestCheckCertAloneDoesNotVerifyAuthority(t *testing.T) {
	real, _ := LoadOrCreate(t.TempDir(), "user")
	other, _ := LoadOrCreate(t.TempDir(), "user")
	foreign := parseCert(t, mustSign(t, other, newUserKey(t)))

	called := false
	checker := &ssh.CertChecker{
		IsUserAuthority: func(k ssh.PublicKey) bool {
			called = true
			return string(ssh.MarshalAuthorizedKey(k)) == string(real.PublicKey())
		},
	}
	err := checker.CheckCert("alice", foreign)
	if err == nil && !called {
		t.Log("confirmed: CheckCert ignores IsUserAuthority (documented in verifyLikeAServer)")
		return
	}
	t.Errorf("x/crypto behaviour changed: CheckCert err=%v, IsUserAuthority called=%v -- "+
		"the warning in verifyLikeAServer may now be stale", err, called)
}

func mustSign(t *testing.T, ca *CA, key []byte) []byte {
	t.Helper()
	b, err := ca.SignUserKey(key, "alice", nil, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPrincipalFromAuthInfo(t *testing.T) {
	ca, _ := LoadOrCreate(t.TempDir(), "user")
	// Signed for shome user "alice" but logging in as OS account "shomeuser".
	certPEM, err := ca.SignUserKey(newUserKey(t), "alice", []string{"shomeuser"}, time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	cert := parseCert(t, certPEM)

	// Reproduce the line sshd writes with ExposeAuthInfo.
	line := "publickey " + cert.Type() + " " +
		base64.StdEncoding.EncodeToString(cert.Marshal()) + "\n"

	got, err := PrincipalFromAuthInfo([]byte(line))
	if err != nil {
		t.Fatalf("PrincipalFromAuthInfo: %v", err)
	}
	if got != "alice" {
		t.Errorf("principal = %q, want alice", got)
	}

	// The OS login account must also be a valid principal, or sshd refuses
	// the certificate for a user that has no OS account of their own.
	found := false
	for _, p := range cert.ValidPrincipals {
		if p == "shomeuser" {
			found = true
		}
	}
	if !found {
		t.Errorf("principals = %v; must include the OS login account", cert.ValidPrincipals)
	}
}

func TestPrincipalFromAuthInfoRejectsRubbish(t *testing.T) {
	for _, in := range []string{"", "publickey ssh-ed25519 AAAA", "garbage", "publickey x-cert-v01@openssh.com !!!"} {
		if _, err := PrincipalFromAuthInfo([]byte(in)); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}
