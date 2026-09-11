package main

import "testing"

// splitArgs is the parser standing between a remote user and the controller's
// OS account, so its rejections matter more than its acceptances.
func TestSplitArgsRejectsShellMetacharacters(t *testing.T) {
	for _, bad := range []string{
		"squeue; rm -rf /",
		"squeue && cat /etc/passwd",
		"squeue | sh",
		"squeue $(whoami)",
		"squeue `id`",
		"sbatch job.sh > /etc/cron.d/x",
		"sbatch *.sh",
		"squeue\nrm -rf /",
		`squeue \; id`,
	} {
		if _, err := splitArgs(bad); err == nil {
			t.Errorf("splitArgs(%q) accepted; metacharacters must be refused", bad)
		}
	}
}

func TestSplitArgsHandlesOrdinaryCommands(t *testing.T) {
	cases := map[string][]string{
		"squeue":                       {"squeue"},
		"squeue -u alice":              {"squeue", "-u", "alice"},
		`sbatch --job-name "my job" x`: {"sbatch", "--job-name", "my job", "x"},
		"  sinfo   ":                   {"sinfo"},
		"":                             nil,
	}
	for in, want := range cases {
		got, err := splitArgs(in)
		if err != nil {
			t.Errorf("splitArgs(%q): %v", in, err)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("splitArgs(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("splitArgs(%q) = %v, want %v", in, got, want)
				break
			}
		}
	}
}

func TestOnlyAllowlistedVerbs(t *testing.T) {
	// Anything that could reach a shell or the filesystem must be absent.
	for _, forbidden := range []string{
		"sh", "bash", "zsh", "exec", "eval", "scp", "sftp", "rsync",
		"ssh", "python3", "curl", "shomed", "shomectld", "admin", "shutdown",
	} {
		if allowed[forbidden] {
			t.Errorf("%q is in the allow-list; it should not be reachable from the login shell", forbidden)
		}
	}
	// And the ordinary user verbs must be present, or the shell is useless.
	for _, want := range []string{"sbatch", "squeue", "scancel", "sinfo", "sacct"} {
		if !allowed[want] {
			t.Errorf("%q missing from the allow-list", want)
		}
	}
}
