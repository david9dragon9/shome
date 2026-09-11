// Package pki issues and verifies the certificates that secure the control
// plane between the controller and its node agents.
//
// Two deliberate choices:
//
//   - The controller is its own CA. A home cluster has no external PKI, and
//     asking someone to run one would guarantee the feature goes unused.
//   - Node certificates are long-lived but individually revocable, while the
//     tokens that mint them are single-use and short-lived. The token is the
//     thing a human copies between machines, so it is the thing that must not
//     be reusable if it leaks into a shell history.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	caValidity   = 10 * 365 * 24 * time.Hour
	certValidity = 2 * 365 * 24 * time.Hour
	orgName      = "shome"
)

// CA is the controller's certificate authority.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	dir  string
}

func keyPath(dir string) string  { return filepath.Join(dir, "ca.key") }
func certPath(dir string) string { return filepath.Join(dir, "ca.crt") }

// LoadOrCreateCA loads the CA from dir, creating it on first run.
func LoadOrCreateCA(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if kb, err := os.ReadFile(keyPath(dir)); err == nil {
		cb, err := os.ReadFile(certPath(dir))
		if err != nil {
			return nil, fmt.Errorf("CA key exists but cert is missing: %w", err)
		}
		key, err := parseKey(kb)
		if err != nil {
			return nil, err
		}
		cert, err := parseCert(cb)
		if err != nil {
			return nil, err
		}
		return &CA{cert: cert, key: key, dir: dir}, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{orgName}, CommonName: "shome cluster CA"},
		NotBefore:             time.Now().Add(-time.Hour), // tolerate modest clock skew between home machines
		NotAfter:              time.Now().Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	if err := writeKey(keyPath(dir), key); err != nil {
		return nil, err
	}
	if err := writePEM(certPath(dir), "CERTIFICATE", der, 0o644); err != nil {
		return nil, err
	}
	return &CA{cert: cert, key: key, dir: dir}, nil
}

func (c *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})
}

func (c *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c.cert)
	return p
}

// Issue signs a certificate for name. hosts are the SANs; for a node agent
// this is empty (it is a client), for the controller it is every address an
// agent might dial.
func (c *CA) Issue(name string, hosts []string, client bool) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	usage := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	if client {
		usage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{orgName}, CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(certValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  usage,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), nil
}

// ServerTLS returns a config requiring a client certificate from our CA.
func (c *CA) ServerTLS(certPEM, keyPEM []byte) (*tls.Config, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientAuth:   tls.RequireAndVerifyClientCert, // mutual auth: an agent must prove itself too
		ClientCAs:    c.Pool(),
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientTLS builds an agent-side config that verifies the controller against
// the CA and presents the agent's own certificate.
func ClientTLS(caPEM, certPEM, keyPEM []byte, serverName string) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA certificate is not valid PEM")
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// PeerName returns the CommonName of the verified client certificate, which is
// how the controller identifies which node is calling.
func PeerName(chains [][]*x509.Certificate) string {
	for _, chain := range chains {
		if len(chain) > 0 {
			return chain[0].Subject.CommonName
		}
	}
	return ""
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode)
}

func writeKey(path string, key *ecdsa.PrivateKey) error {
	b, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return writePEM(path, "EC PRIVATE KEY", b, 0o600)
}

func parseKey(b []byte) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("bad PEM in key")
	}
	return x509.ParseECPrivateKey(blk.Bytes)
}

func parseCert(b []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("bad PEM in certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}

// parseCertDER is used by tests to inspect an issued certificate.
func parseCertDER(der []byte) (*x509.Certificate, error) { return x509.ParseCertificate(der) }

// ServerTLSJoinable is like ServerTLS but accepts connections without a client
// certificate, so an agent that has none yet can reach the join endpoint.
//
// The guarantee moves from the TLS layer to the handler: every endpoint except
// /agent/join must call RequirePeer. That is a sharper edge than
// RequireAndVerifyClientCert, so it is enforced by a single helper and covered
// by a test that an uncertified client cannot heartbeat.
func (c *CA) ServerTLSJoinable(certPEM, keyPEM []byte) (*tls.Config, error) {
	cfg, err := c.ServerTLS(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	cfg.ClientAuth = tls.VerifyClientCertIfGiven
	return cfg, nil
}
