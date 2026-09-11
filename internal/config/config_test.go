package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A tiny document so the engine can be tested without a real settings file.
type doc struct {
	Name  string
	Count int
	Size  float64
	Mode  string
	Tags  []string
}

func testSchema(t *testing.T) (*Schema, *doc) {
	t.Helper()
	d := &doc{}
	path := filepath.Join(t.TempDir(), "settings")
	return &Schema{
		Title: "test settings", Command: "test config",
		Path: func() string { return path },
		Load: func() (any, error) { return d, nil },
		Save: func(any) error { return os.WriteFile(path, []byte("saved\n"), 0o600) },
		Fields: []Field{
			{Name: "name", Kind: String,
				Get:   func(a any) (string, bool) { return a.(*doc).Name, a.(*doc).Name != "" },
				Set:   func(a any, v string) error { a.(*doc).Name = v; return nil },
				Unset: func(a any) { a.(*doc).Name = "" }},
			{Name: "count", Kind: Int,
				Get: func(a any) (string, bool) { return FormatInt(a.(*doc).Count, true), a.(*doc).Count != 0 },
				Set: func(a any, v string) error {
					n, err := ParseInt(v)
					if err != nil {
						return err
					}
					a.(*doc).Count = n
					return nil
				},
				Unset: func(a any) { a.(*doc).Count = 0 }},
			{Name: "mode", Kind: Enum, Values: []string{"on", "off"},
				Get: func(a any) (string, bool) { return a.(*doc).Mode, a.(*doc).Mode != "" },
				Set: func(a any, v string) error {
					m, err := ParseEnum(v, []string{"on", "off"})
					if err != nil {
						return err
					}
					a.(*doc).Mode = m
					return nil
				}},
		},
	}, d
}

func TestSetAndUnset(t *testing.T) {
	s, d := testSchema(t)
	if err := s.Run([]string{"set", "count", "5"}); err != nil {
		t.Fatal(err)
	}
	if d.Count != 5 {
		t.Errorf("Count = %d, want 5", d.Count)
	}
	if err := s.Run([]string{"unset", "count"}); err != nil {
		t.Fatal(err)
	}
	if d.Count != 0 {
		t.Errorf("Count = %d after unset", d.Count)
	}
}

// A field with no Unset must say so rather than silently doing nothing.
func TestUnsetRefusedWhereUnsupported(t *testing.T) {
	s, _ := testSchema(t)
	err := s.Run([]string{"unset", "mode"})
	if err == nil {
		t.Fatal("unset succeeded on a field that does not support it")
	}
	if !strings.Contains(err.Error(), "cannot be unset") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestValidationRejectsBadValues(t *testing.T) {
	s, d := testSchema(t)
	if err := s.Run([]string{"set", "count", "-4"}); err == nil {
		t.Error("a negative count was accepted")
	}
	if err := s.Run([]string{"set", "mode", "maybe"}); err == nil {
		t.Error("an invalid enum value was accepted")
	}
	// A rejected value must not be half-applied.
	if d.Mode != "" {
		t.Errorf("Mode = %q after a rejected set", d.Mode)
	}
}

// A dotted name is easy to mistype and hard to spot, so an unknown name
// suggests the closest match rather than only listing everything.
func TestUnknownNameSuggests(t *testing.T) {
	s, _ := testSchema(t)
	err := s.Run([]string{"set", "cont", "3"})
	if err == nil {
		t.Fatal("an unknown name was accepted")
	}
	if !strings.Contains(err.Error(), "count") {
		t.Errorf("no suggestion offered: %v", err)
	}
}

// A bare name is a common thing to type and should read the value.
func TestBareNameReadsValue(t *testing.T) {
	s, d := testSchema(t)
	d.Name = "mini"
	if err := s.Run([]string{"name"}); err != nil {
		t.Errorf("a bare name was not treated as a read: %v", err)
	}
}

func TestParsers(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want float64
		bad  bool
	}{
		{"200G", 200, false}, {"512M", 0.5, false}, {"2T", 2048, false},
		{"1", 1, false}, {"unlimited", 0, false}, {"", 0, false},
		{"-5G", 0, true}, {"lots", 0, true},
	} {
		got, err := ParseSizeGB(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("ParseSizeGB(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSizeGB(%q): %v", tc.in, err)
		} else if got != tc.want {
			t.Errorf("ParseSizeGB(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// Bare seconds are accepted because people type them.
	if d, err := ParseDuration("30"); err != nil || d != 30*time.Second {
		t.Errorf("ParseDuration(30) = %v, %v", d, err)
	}
	if _, err := ParseDuration("-5s"); err == nil {
		t.Error("a negative duration was accepted")
	}
	for _, y := range []string{"true", "yes", "on", "1"} {
		if v, err := ParseBool(y); err != nil || !v {
			t.Errorf("ParseBool(%q) = %v, %v", y, v, err)
		}
	}
	if _, err := ParseBool("maybe"); err == nil {
		t.Error("ParseBool accepted a non-boolean")
	}
}

func TestFormatSizeGB(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, "unlimited"}, {200, "200G"}, {0.5, "512M"}, {2048, "2.0T"},
	} {
		if got := FormatSizeGB(tc.in); got != tc.want {
			t.Errorf("FormatSizeGB(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Setting one field must not disturb another: the engine reads, modifies and
// writes rather than replacing the document from a partial struct.
func TestSetPreservesOtherFields(t *testing.T) {
	s, d := testSchema(t)
	s.Run([]string{"set", "name", "mini"})
	s.Run([]string{"set", "count", "7"})
	if d.Name != "mini" || d.Count != 7 {
		t.Errorf("got %+v; setting one field disturbed another", d)
	}
}

// A half-life or an availability window is naturally expressed in days, and
// refusing "7d" while defaulting to a week makes the config surface feel
// arbitrary. time.ParseDuration knows nothing longer than an hour.
func TestParseDurationAcceptsDaysAndWeeks(t *testing.T) {
	day := 24 * time.Hour
	for in, want := range map[string]time.Duration{
		"7d":    7 * day,
		"1d":    day,
		"2w":    14 * day,
		"1w":    7 * day,
		"1d12h": day + 12*time.Hour,
		"1w2d":  9 * day,
		// The shorter forms must keep working exactly as before.
		"30s":  30 * time.Second,
		"5m":   5 * time.Minute,
		"2h":   2 * time.Hour,
		"90":   90 * time.Second,
		"":     0,
		"none": 0,
	} {
		got, err := ParseDuration(in)
		if err != nil {
			t.Errorf("ParseDuration(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseDuration(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"d", "w", "xd", "-5d", "1d5x", "tomorrow"} {
		if got, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) = %v, want an error", bad, got)
		}
	}
}
