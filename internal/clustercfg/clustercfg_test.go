package clustercfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingFileIsNotAnError(t *testing.T) {
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("an unnamed cluster should load cleanly: %v", err)
	}
	if c.ClusterName() != DefaultName {
		t.Errorf("ClusterName() = %q, want %q", c.ClusterName(), DefaultName)
	}
	if got := c.LabelOf("some-machine"); got != "some-machine" {
		t.Errorf("LabelOf with no config = %q, want the identity back", got)
	}
}

func TestRoundTrip(t *testing.T) {
	root := t.TempDir()
	in := Config{Name: "home", Nodes: map[string]Node{
		"a-long-hostname": {Label: "mini", Note: "in the study"},
		"YW-MacBook-Pro":  {Label: "intel-mac"},
	}}
	if err := Save(root, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if out.ClusterName() != "home" {
		t.Errorf("name = %q", out.ClusterName())
	}
	if out.LabelOf("a-long-hostname") != "mini" {
		t.Errorf("label = %q", out.LabelOf("a-long-hostname"))
	}
	if out.NoteOf("a-long-hostname") != "in the study" {
		t.Errorf("note = %q", out.NoteOf("a-long-hostname"))
	}
	// The file is meant to be hand-edited, so it must be readable.
	b, _ := os.ReadFile(File(root))
	if !strings.Contains(string(b), "# shome cluster identity") {
		t.Error("no explanatory header for someone opening the file")
	}
}

// Naming a machine "mini" must make "mini" work everywhere a machine name is
// accepted, or the label is decoration rather than a name.
func TestResolveAcceptsLabelsAndIdentities(t *testing.T) {
	c := Config{Nodes: map[string]Node{
		"a-long-hostname": {Label: "mini"},
		"linux-box":       {Label: "gpu"},
	}}
	for in, want := range map[string]string{
		"mini":            "a-long-hostname",
		"MINI":            "a-long-hostname",
		"a-long-hostname": "a-long-hostname",
		"gpu":             "linux-box",
		"linux-box":       "linux-box",
		// Unknown names come back unchanged: whether a machine exists is for
		// the caller that has the list to say.
		"nosuch": "nosuch",
		"":       "",
	} {
		if got := c.Resolve(in); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", in, got, want)
		}
	}
}

// An identity must win over another machine's label, or labelling one machine
// with another's real name would silently redirect commands.
func TestIdentityWinsOverAnotherMachinesLabel(t *testing.T) {
	c := Config{Nodes: map[string]Node{
		"alpha": {Label: "beta"},
		"beta":  {Label: "gamma"},
	}}
	if got := c.Resolve("beta"); got != "beta" {
		t.Errorf("Resolve(\"beta\") = %q; a real identity must win over a label", got)
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"home", "lab-2", "my_cluster", "a.b", "X1"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("ValidName(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "has space", "sl/ash", "semi;colon", "quote\"", strings.Repeat("x", 41)} {
		if err := ValidName(bad); err == nil {
			t.Errorf("ValidName(%q) = nil, want an error", bad)
		}
	}
}

func TestSaveDropsEmptyEntries(t *testing.T) {
	root := t.TempDir()
	if err := Save(root, Config{Name: "home", Nodes: map[string]Node{
		"keep":  {Label: "kept"},
		"empty": {},
	}}); err != nil {
		t.Fatal(err)
	}
	out, _ := Load(root)
	if _, ok := out.Nodes["empty"]; ok {
		t.Error("an entry with no label and no note was written out")
	}
	if out.LabelOf("keep") != "kept" {
		t.Error("clearing empties dropped a real entry")
	}
}

// A typo must not silently rename the cluster back to its default everywhere.
func TestCacheKeepsWhatWorksWhenTheFileBreaks(t *testing.T) {
	root := t.TempDir()
	if err := Save(root, Config{Name: "home"}); err != nil {
		t.Fatal(err)
	}
	cache := NewCache(root)
	if got := cache.Get(nil).ClusterName(); got != "home" {
		t.Fatalf("first read = %q", got)
	}
	if err := os.WriteFile(File(root), []byte("name: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := cache.Get(nil).ClusterName(); got != "home" {
		t.Errorf("after a broken edit = %q, want the last good value %q", got, "home")
	}
}

// Hand-editing the file must take effect without a restart.
func TestCacheReloadsOnChange(t *testing.T) {
	root := t.TempDir()
	Save(root, Config{Name: "before"})
	cache := NewCache(root)
	if got := cache.Get(nil).ClusterName(); got != "before" {
		t.Fatalf("got %q", got)
	}
	// Size differs, so this is detected regardless of timestamp granularity.
	if err := os.WriteFile(File(root), []byte("name: afterwards\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := cache.Get(nil).ClusterName(); got != "afterwards" {
		t.Errorf("after editing the file = %q, want %q", got, "afterwards")
	}
}

func TestFilePath(t *testing.T) {
	if got, want := File("/r"), filepath.Join("/r", "cluster.yaml"); got != want {
		t.Errorf("File() = %q, want %q", got, want)
	}
}

// A name must resolve to exactly one machine, or which one a command reaches
// would depend on map iteration order.
func TestCanLabelRejectsAmbiguity(t *testing.T) {
	c := Config{Nodes: map[string]Node{
		"alpha-host": {Label: "alpha"},
		"beta-host":  {Label: "beta"},
	}}
	ids := []string{"alpha-host", "beta-host", "gamma-host"}

	// Another machine's label.
	if err := c.CanLabel("gamma-host", "alpha", ids); err == nil {
		t.Error("a machine was allowed to take another's label")
	}
	// Another machine's identity.
	if err := c.CanLabel("gamma-host", "beta-host", ids); err == nil {
		t.Error("a machine was allowed to take another's identity as its label")
	}
	// Case does not make it a different name.
	if err := c.CanLabel("gamma-host", "ALPHA", ids); err == nil {
		t.Error("case alone was treated as a distinct name")
	}
	// Its own label again is fine -- setting a name twice is not a conflict.
	if err := c.CanLabel("alpha-host", "alpha", ids); err != nil {
		t.Errorf("re-setting a machine's own label was refused: %v", err)
	}
	// A free name is fine.
	if err := c.CanLabel("gamma-host", "gamma", ids); err != nil {
		t.Errorf("a free name was refused: %v", err)
	}
	// An invalid name is still invalid.
	if err := c.CanLabel("gamma-host", "has space", ids); err == nil {
		t.Error("an invalid name passed CanLabel")
	}
}

// Labelling a machine with its own identity says nothing, so it is stored as
// no label rather than as a stanza that looks like a setting and is not.
func TestNormaliseLabel(t *testing.T) {
	for _, c := range []struct{ identity, in, want string }{
		{"a-long-hostname", "a-long-hostname", ""},
		{"a-long-hostname", "a-long-hostname", ""},
		{"a-long-hostname", "  a-long-hostname  ", ""},
		{"a-long-hostname", "mini", "mini"},
		{"a-long-hostname", "  mini ", "mini"},
		{"host", "", ""},
	} {
		if got := NormaliseLabel(c.identity, c.in); got != c.want {
			t.Errorf("NormaliseLabel(%q, %q) = %q, want %q", c.identity, c.in, got, c.want)
		}
	}
}
