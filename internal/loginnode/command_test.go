package loginnode

import (
	"strings"
	"testing"
)

func TestSplitCommandQuoting(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"squeue", []string{"squeue"}},
		{"sbatch --mem 8G job.sh", []string{"sbatch", "--mem", "8G", "job.sh"}},
		{`sbatch --job-name "my job"`, []string{"sbatch", "--job-name", "my job"}},
		{`sbatch --job-name 'my job'`, []string{"sbatch", "--job-name", "my job"}},
		{"  squeue   -a  ", []string{"squeue", "-a"}},
		{"", nil},
		{`sbatch --name ""`, []string{"sbatch", "--name", ""}},
	} {
		got, err := splitCommand(tc.in)
		if err != nil {
			t.Errorf("splitCommand(%q): %v", tc.in, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("splitCommand(%q) = %q, want %q", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitCommand(%q) = %q, want %q", tc.in, got, tc.want)
				break
			}
		}
	}
}

// Metacharacters are refused rather than escaped. Nothing downstream
// interprets them today, but a parser that silently accepts them leaves no
// safe place for a future convenience to fail.
func TestSplitCommandRefusesShellMetacharacters(t *testing.T) {
	for _, in := range []string{
		"squeue; rm -rf /",
		"squeue && id",
		"squeue | sh",
		"squeue > /tmp/x",
		"squeue < /etc/passwd",
		"echo $(id)",
		"echo `id`",
		"squeue $HOME",
		"squeue &",
		"cat /etc/*",
		"squeue\nid",
		"squeue\rid",
		`squeue \; id`,
	} {
		if got, err := splitCommand(in); err == nil {
			t.Errorf("splitCommand(%q) was accepted as %q", in, got)
		}
	}
}

func TestSplitCommandUnclosedQuote(t *testing.T) {
	if _, err := splitCommand(`sbatch --name "oops`); err == nil {
		t.Error("an unclosed quote was accepted")
	} else if !strings.Contains(err.Error(), "quote") {
		t.Errorf("unhelpful error: %v", err)
	}
}
