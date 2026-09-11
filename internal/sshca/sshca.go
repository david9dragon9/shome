// Package sshca issues the SSH certificates that let users reach the cluster
// the way they would an academic HPC login node.
//
// Certificates rather than authorized_keys, deliberately: they are short-lived,
// so nothing long-lived sits on a user's laptop, and revocation is a single
// list on the login node rather than a hunt through per-user key files.
//
// Two separate CAs, for host and user. A single CA that signs both would let a
// stolen host key mint user certificates.
package sshca

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// UserCertValidity is how long an issued user certificate lasts. Short by
// design: it is the credential a laptop carries around.
const UserCertValidity = 12 * time.Hour

// KeyIDPrefix marks a certificate's KeyId as carrying a shome identity.
const KeyIDPrefix = "shome:"

// PrincipalFromAuthInfo extracts the shome identity from the file sshd writes
// when ExposeAuthInfo is enabled.
//
// The file holds lines like:
//
//	publickey ssh-ed25519-cert-v01@openssh.com AAAA<base64 certificate>
//
// The identity is inside the certificate blob, not in the plain text, so the
// blob must be parsed. This is trustworthy precisely because sshd has already
// verified the certificate's signature and validity before writing it.
func PrincipalFromAuthInfo(contents []byte) (string, error) {
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "publickey" {
			continue
		}
		if !strings.Contains(fields[1], "cert-v01@openssh.com") {
			continue
		}
		blob, err := base64.StdEncoding.DecodeString(fields[2])
		if err != nil {
			continue
		}
		pk, err := ssh.ParsePublicKey(blob)
		if err != nil {
			continue
		}
		cert, ok := pk.(*ssh.Certificate)
		if !ok {
			continue
		}
		if name, ok := strings.CutPrefix(cert.KeyId, KeyIDPrefix); ok && name != "" {
			return name, nil
		}
	}
	return "", fmt.Errorf("no shome certificate found in the SSH auth info")
}

// CA holds one signing key pair.
type CA struct {
	signer ssh.Signer
	dir    string
	name   string
}

// LoadOrCreate loads the named CA from dir, creating it on first use.
func LoadOrCreate(dir, name string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	keyFile := filepath.Join(dir, name+"_ca")
	if b, err := os.ReadFile(keyFile); err == nil {
		signer, err := ssh.ParsePrivateKey(b)
		if err != nil {
			return nil, fmt.Errorf("parse %s CA key: %w", name, err)
		}
		return &CA{signer: signer, dir: dir, name: name}, nil
	}

	// Ed25519: small, fast, and universally supported by modern OpenSSH.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := ssh.MarshalPrivateKey(priv, "shome "+name+" CA")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(der), 0o600); err != nil {
		return nil, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyFile+".pub", ssh.MarshalAuthorizedKey(sshPub), 0o644); err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	return &CA{signer: signer, dir: dir, name: name}, nil
}

// PublicKey returns the CA's public key in authorized_keys form, for
// TrustedUserCAKeys or a known_hosts @cert-authority line.
func (c *CA) PublicKey() []byte {
	return ssh.MarshalAuthorizedKey(c.signer.PublicKey())
}

func (c *CA) PublicKeyPath() string { return filepath.Join(c.dir, c.name+"_ca.pub") }

// SignUserKey issues a user certificate for the given principal.
//
// Extensions are minimal on purpose: no port forwarding, no agent forwarding,
// no X11, no pty by default. A login-node session exists to run shome
// commands, and every extension is a way out of that.
// The shome identity travels in KeyId, not in the principal list, because
// sshd requires one of the principals to equal the OS login name -- and shome
// users are not OS users (see docs/design-notes.md). Certificates therefore carry the OS login
// account as a principal so sshd is satisfied, while shome-shell reads the
// real identity out of KeyId, which sshd has already verified as part of the
// signed certificate.
func (c *CA) SignUserKey(pubKey []byte, principal string, loginAccounts []string, validity time.Duration, allowPTY bool) ([]byte, error) {
	pk, _, _, _, err := ssh.ParseAuthorizedKey(pubKey)
	if err != nil {
		return nil, fmt.Errorf("not a valid SSH public key: %w", err)
	}
	serial, err := randUint64()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	cert := &ssh.Certificate{
		Key:             pk,
		Serial:          serial,
		CertType:        ssh.UserCert,
		KeyId:           KeyIDPrefix + principal,
		ValidPrincipals: append([]string{principal}, loginAccounts...),
		// A minute of leeway for modest clock skew between home machines.
		ValidAfter:  uint64(now.Add(-time.Minute).Unix()),
		ValidBefore: uint64(now.Add(validity).Unix()),
	}
	if allowPTY {
		cert.Permissions.Extensions = map[string]string{"permit-pty": ""}
	}
	if err := cert.SignCert(rand.Reader, c.signer); err != nil {
		return nil, err
	}
	return ssh.MarshalAuthorizedKey(cert), nil
}

// SignHostKey issues a host certificate so users never see a TOFU prompt and
// cannot be MITM'd on first connect.
func (c *CA) SignHostKey(pubKey []byte, hosts []string, validity time.Duration) ([]byte, error) {
	pk, _, _, _, err := ssh.ParseAuthorizedKey(pubKey)
	if err != nil {
		return nil, fmt.Errorf("not a valid SSH public key: %w", err)
	}
	serial, err := randUint64()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	cert := &ssh.Certificate{
		Key:             pk,
		Serial:          serial,
		CertType:        ssh.HostCert,
		KeyId:           "shome-login-node",
		ValidPrincipals: hosts,
		ValidAfter:      uint64(now.Add(-time.Minute).Unix()),
		ValidBefore:     uint64(now.Add(validity).Unix()),
	}
	if err := cert.SignCert(rand.Reader, c.signer); err != nil {
		return nil, err
	}
	return ssh.MarshalAuthorizedKey(cert), nil
}

func randUint64() (uint64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v, nil
}
