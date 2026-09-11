package pki

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newCA(t *testing.T) *CA {
	t.Helper()
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func TestCAPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A regenerated CA would invalidate every node cert on controller restart.
	if string(a.CertPEM()) != string(b.CertPEM()) {
		t.Error("CA changed on reload; all node certs would be invalidated")
	}
}

// The core guarantee: mTLS both ways. A client with no cert, or a cert from a
// different CA, must be rejected.
func TestMutualTLSRejectsOutsiders(t *testing.T) {
	ca := newCA(t)
	srvCert, srvKey, err := ca.Issue("controller", []string{"127.0.0.1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := ca.ServerTLS(srvCert, srvKey)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, PeerName(r.TLS.VerifiedChains))
	}))
	srv.TLS = tlsCfg
	srv.StartTLS()
	defer srv.Close()

	t.Run("valid node cert is accepted and identifies itself", func(t *testing.T) {
		c, k, err := ca.Issue("node-a", nil, true)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := ClientTLS(ca.CertPEM(), c, k, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		cli := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		resp, err := cli.Get(srv.URL)
		if err != nil {
			t.Fatalf("valid client rejected: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if string(b) != "node-a" {
			t.Errorf("server saw peer %q, want node-a", b)
		}
	})

	t.Run("no client cert is rejected", func(t *testing.T) {
		cli := &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: ca.Pool(), ServerName: "127.0.0.1"}}}
		if _, err := cli.Get(srv.URL); err == nil {
			t.Error("server accepted a client with no certificate")
		}
	})

	t.Run("cert from a foreign CA is rejected", func(t *testing.T) {
		other := newCA(t)
		c, k, err := other.Issue("impostor", nil, true)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := ClientTLS(ca.CertPEM(), c, k, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		cli := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		if _, err := cli.Get(srv.URL); err == nil {
			t.Error("server accepted a certificate signed by an unrelated CA")
		}
	})

	t.Run("client rejects a controller not signed by our CA", func(t *testing.T) {
		other := newCA(t)
		c, k, _ := ca.Issue("node-b", nil, true)
		// Trust only the OTHER CA, so our real server should fail verification.
		cfg, err := ClientTLS(other.CertPEM(), c, k, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		cli := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		if _, err := cli.Get(srv.URL); err == nil {
			t.Error("agent accepted an untrusted controller; this is the MITM case")
		}
	})
}

func TestIssuedCertsHaveCorrectUsage(t *testing.T) {
	ca := newCA(t)
	// A client cert must not be usable to impersonate the controller.
	cPEM, kPEM, err := ca.Issue("node-a", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(cPEM, kPEM)
	if err != nil {
		t.Fatal(err)
	}
	leaf := pair.Certificate[0]
	parsed, err := parseCertDER(leaf)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range parsed.ExtKeyUsage {
		if u == 1 { // ExtKeyUsageServerAuth
			t.Error("node client cert carries ServerAuth; it could impersonate the controller")
		}
	}
	if parsed.IsCA {
		t.Error("node cert is a CA; it could mint further certs")
	}
}
