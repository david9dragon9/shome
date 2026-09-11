package ctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidwu/shome/internal/store"
)

func tokenFixture(t *testing.T) (*store.Store, string) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "shome.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, root
}

func TestEnsureAdminTokenBootstrapsEmptyCluster(t *testing.T) {
	st, root := tokenFixture(t)
	wrote, err := EnsureAdminToken(t.Context(), st, root)
	if err != nil || !wrote {
		t.Fatalf("EnsureAdminToken = (%v, %v), want (true, nil)", wrote, err)
	}
	b, err := os.ReadFile(AdminTokenPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if u, err := st.UserByToken(t.Context(), strings.TrimSpace(string(b))); err != nil {
		t.Fatalf("written token does not authenticate: %v", err)
	} else if u.Role != store.RoleAdmin {
		t.Fatalf("bootstrap account has role %q", u.Role)
	}
}

func TestEnsureAdminTokenIsIdempotent(t *testing.T) {
	st, root := tokenFixture(t)
	EnsureAdminToken(t.Context(), st, root)
	first, _ := os.ReadFile(AdminTokenPath(root))

	wrote, err := EnsureAdminToken(t.Context(), st, root)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Error("second call rewrote a token that was already present")
	}
	second, _ := os.ReadFile(AdminTokenPath(root))
	if string(first) != string(second) {
		t.Error("an existing, working token was replaced")
	}
}

// Losing admin.token used to lock the owner out of their own cluster
// permanently: tokens are stored hashed, so the original could not be
// recovered and nothing would reissue one.
func TestEnsureAdminTokenReissuesAfterFileLoss(t *testing.T) {
	st, root := tokenFixture(t)
	EnsureAdminToken(t.Context(), st, root)
	old, _ := os.ReadFile(AdminTokenPath(root))
	if err := os.Remove(AdminTokenPath(root)); err != nil {
		t.Fatal(err)
	}

	wrote, err := EnsureAdminToken(t.Context(), st, root)
	if err != nil || !wrote {
		t.Fatalf("EnsureAdminToken = (%v, %v), want (true, nil)", wrote, err)
	}
	fresh, err := os.ReadFile(AdminTokenPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if string(fresh) == string(old) {
		t.Error("reissue produced the same token; it should be a new secret")
	}
	if _, err := st.UserByToken(t.Context(), strings.TrimSpace(string(fresh))); err != nil {
		t.Fatalf("reissued token does not authenticate: %v", err)
	}
	// The old one must stop working, or a leaked token would outlive its reset.
	if _, err := st.UserByToken(t.Context(), strings.TrimSpace(string(old))); err == nil {
		t.Error("the previous token still authenticates after a reissue")
	}
}
