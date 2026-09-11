//go:build darwin

package darwin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidwu/shome/internal/job"
)

// Regression, and the most consequential one in this package: the profile
// denies the owner's home and other jobs' scratch, but for a while it did not
// deny the shome installation itself. Because the profile grants a broad
// (allow file-read*) and narrows it with denies, everything not denied was
// readable -- including admin.token, which is a bearer credential for the
// whole cluster, and the mTLS CA private key.
//
// A job that can read admin.token is a cluster administrator, so this was a
// complete escape from the boundary the rest of this file builds. It has to
// be tested against a real sandbox: an SBPL rule that matches nothing fails
// silently, so nothing short of running the thing proves it.
func TestJobCannotReadTheClustersSecrets(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	ownerHome := t.TempDir()
	ownerHome, _ = filepath.EvalSymlinks(ownerHome)

	// Lay out an installation the way a controller has one: the agent's own
	// root is a subdirectory, and the secrets sit beside it.
	agentRoot := filepath.Join(root, "local")
	if err := os.MkdirAll(filepath.Join(agentRoot, "jobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pki"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "users", "someone-else"), 0o700); err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{
		filepath.Join(root, "admin.token"):                "ADMIN-TOKEN-SECRET",
		filepath.Join(root, "shome.db"):                   "JOB-DATABASE-SECRET",
		filepath.Join(root, "pki", "ca.key"):              "CA-PRIVATE-KEY-SECRET",
		filepath.Join(root, "qos.yaml"):                   "QOS-SECRET",
		filepath.Join(root, "users", "someone-else", "f"): "ANOTHER-ACCOUNTS-FILE",
	}
	for p, v := range secrets {
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	b := &Backend{Root: agentRoot, OwnerHome: ownerHome, StateRoot: root}
	sbx, err := b.Prepare(context.Background(), job.Spec{Name: "probe"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Cleanup(context.Background(), sbx) })

	run := func(args ...string) string {
		out, _ := exec.Command("sandbox-exec",
			append([]string{"-f", sbx.ProfilePath}, args...)...).CombinedOutput()
		return string(out)
	}

	for p, v := range secrets {
		if out := run("/bin/cat", p); strings.Contains(out, v) {
			t.Errorf("a job read %s -- its contents are %q", p, v)
		}
	}

	// The job must still be able to do its work, or the deny is too broad to
	// ship. This is the half that a careless fix breaks.
	if out := run("/usr/bin/touch", filepath.Join(sbx.ScratchDir, "x")); out != "" {
		t.Errorf("job cannot write its own scratch: %s", out)
	}
	if _, err := os.Stat(filepath.Join(sbx.ScratchDir, "x")); err != nil {
		t.Errorf("scratch write did not land: %v", err)
	}
	if out := run("/bin/sh", "-c", "cd "+sbx.ScratchDir+" && pwd"); !strings.Contains(out, sbx.ScratchDir) {
		t.Errorf("job cannot cd into its own scratch: %s", out)
	}
	if out := run("/bin/cat", "/usr/bin/env"); out == "" {
		t.Error("job cannot read system paths it legitimately needs")
	}
}
