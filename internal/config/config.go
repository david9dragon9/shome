// Package config is one implementation of "edit a settings file from the
// command line", used for every settings file shome has.
//
// There are two of them with more likely to follow -- the machine owner's
// contribution policy and the cluster's own settings -- and they want the same
// six verbs, the same parsing, the same "which of these did I actually set"
// display, and the same care about not dropping fields the caller did not
// mention. Writing that twice would guarantee the two drift, and the drift
// would show up as one file supporting a flag the other silently ignored.
//
// A schema is a list of named fields with a getter, a setter and a parser.
// Everything else -- the verbs, the table, the validation messages, the
// did-you-mean suggestion -- comes from here.
package config

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// Kind determines how a value is parsed and described.
type Kind int

const (
	String Kind = iota
	Int
	Float
	Bool
	Duration
	// Size accepts 8G, 512M, 2T and stores whatever unit the field wants.
	Size
	// List is a comma-separated set of strings.
	List
	// Enum is a String restricted to Values.
	Enum
)

func (k Kind) String() string {
	switch k {
	case Int:
		return "number"
	case Float:
		return "number"
	case Bool:
		return "true|false"
	case Duration:
		return "duration"
	case Size:
		return "size"
	case List:
		return "list"
	case Enum:
		return "one of"
	}
	return "text"
}

// Field is one setting.
type Field struct {
	// Name is dotted and stable: it is what a user types and what appears in
	// documentation, so it must not change when the underlying struct does.
	Name string
	Help string
	Kind Kind
	// Values enumerates the permitted values for Enum.
	Values []string
	// Effect describes when a change takes hold, for settings that are not
	// picked up immediately. Empty means "within seconds".
	Effect string

	// Get returns the current value and whether it was explicitly set.
	// Distinguishing those is the point of the display: "unset" and "set to
	// the same value as the default" behave differently when the default
	// later changes.
	Get func(doc any) (value string, set bool)
	Set func(doc any, v string) error
	// Unset returns a field to its default. Nil means the field cannot be
	// unset, only overwritten.
	Unset func(doc any)
}

// Schema describes one settings file.
type Schema struct {
	// Title names the thing being configured, for headings and errors.
	Title string
	// Command is how this schema is invoked, used in examples.
	Command string
	// Path is the file it reads and writes.
	Path func() string
	// Load reads the file, returning an empty document when it does not exist
	// yet -- a first run must be able to set a value without a file already
	// being there.
	Load func() (any, error)
	Save func(doc any) error
	// Note is printed under the table, for anything the fields cannot say.
	Note   string
	Fields []Field
}

func (s *Schema) field(name string) (Field, bool) {
	name = strings.TrimPrefix(name, "--")
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// Run dispatches one config command.
func (s *Schema) Run(args []string) error {
	if len(args) == 0 {
		return s.Show()
	}
	switch args[0] {
	case "show", "list":
		return s.Show()
	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: %s get NAME", s.Command)
		}
		return s.PrintValue(args[1])
	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: %s set NAME VALUE\n\nSee %s help for the names",
				s.Command, s.Command)
		}
		// Everything after the name joins, so a list or a quoted phrase works
		// without the caller having to think about quoting twice.
		return s.SetValue(args[1], strings.Join(args[2:], " "))
	case "unset", "clear":
		if len(args) < 2 {
			return fmt.Errorf("usage: %s unset NAME", s.Command)
		}
		return s.UnsetValue(args[1])
	case "path", "file":
		fmt.Println(s.Path())
		return nil
	case "edit":
		return s.Edit()
	case "help", "-h", "--help":
		s.Usage()
		return nil
	}
	// A bare name is a common thing to type; treat it as `get`.
	if _, ok := s.field(args[0]); ok {
		return s.PrintValue(args[0])
	}
	s.Usage()
	return fmt.Errorf("unknown setting or command %q%s", args[0], s.suggest(args[0]))
}

// suggest offers the closest field name, because a typo in a dotted name is
// easy to make and hard to spot.
func (s *Schema) suggest(name string) string {
	name = strings.TrimPrefix(name, "--")
	best, bestD := "", 1<<30
	for _, f := range s.Fields {
		if d := editDistance(name, f.Name); d < bestD {
			best, bestD = f.Name, d
		}
	}
	if best != "" && bestD <= 4 {
		return fmt.Sprintf("\n\nDid you mean %q?", best)
	}
	return ""
}

// Usage prints the verbs and every setting.
func (s *Schema) Usage() {
	fmt.Printf("%s - %s\n\n", s.Command, s.Title)
	fmt.Printf("  %s                     show every setting and its value\n", s.Command)
	fmt.Printf("  %s get NAME            one value\n", s.Command)
	fmt.Printf("  %s set NAME VALUE      change it\n", s.Command)
	fmt.Printf("  %s unset NAME          back to the default\n", s.Command)
	fmt.Printf("  %s path                where the file lives\n", s.Command)
	fmt.Printf("  %s edit                open it in $EDITOR\n", s.Command)
	fmt.Printf("\nsettings:\n")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, f := range s.Fields {
		kind := f.Kind.String()
		if f.Kind == Enum {
			kind = strings.Join(f.Values, "|")
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\n", f.Name, kind, f.Help)
	}
	w.Flush()
	if s.Note != "" {
		fmt.Printf("\n%s\n", s.Note)
	}
}

// Show prints every setting, its value, and whether it was set here.
func (s *Schema) Show() error {
	doc, err := s.Load()
	if err != nil {
		return err
	}
	fmt.Printf("%s\n\n", s.Title)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SETTING\tVALUE\tSOURCE")
	for _, f := range s.Fields {
		v, set := f.Get(doc)
		src := "default"
		if set {
			src = "set here"
		}
		if v == "" {
			v = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", f.Name, v, src)
	}
	w.Flush()
	fmt.Printf("\nfile: %s\n", s.Path())
	if s.Note != "" {
		fmt.Printf("%s\n", s.Note)
	}
	return nil
}

// PrintValue prints one setting, bare, so it can be used in a script.
func (s *Schema) PrintValue(name string) error {
	f, ok := s.field(name)
	if !ok {
		return fmt.Errorf("no setting %q%s", name, s.suggest(name))
	}
	doc, err := s.Load()
	if err != nil {
		return err
	}
	v, _ := f.Get(doc)
	fmt.Println(v)
	return nil
}

// SetValue parses and stores one setting.
//
// Read-modify-write, deliberately: setting one field must not drop the others,
// which a whole-document write from a partial struct would do.
func (s *Schema) SetValue(name, raw string) error {
	f, ok := s.field(name)
	if !ok {
		return fmt.Errorf("no setting %q%s", name, s.suggest(name))
	}
	doc, err := s.Load()
	if err != nil {
		return err
	}
	if err := f.Set(doc, raw); err != nil {
		return fmt.Errorf("%s: %w", f.Name, err)
	}
	if err := s.Save(doc); err != nil {
		return err
	}
	v, _ := f.Get(doc)
	fmt.Printf("%s = %s\n", f.Name, v)
	if f.Effect != "" {
		fmt.Printf("\n%s\n", f.Effect)
	}
	return nil
}

// UnsetValue returns one setting to its default.
func (s *Schema) UnsetValue(name string) error {
	f, ok := s.field(name)
	if !ok {
		return fmt.Errorf("no setting %q%s", name, s.suggest(name))
	}
	if f.Unset == nil {
		return fmt.Errorf("%s cannot be unset; set it to a different value instead", f.Name)
	}
	doc, err := s.Load()
	if err != nil {
		return err
	}
	f.Unset(doc)
	if err := s.Save(doc); err != nil {
		return err
	}
	v, _ := f.Get(doc)
	fmt.Printf("%s is back to its default (%s)\n", f.Name, v)
	return nil
}

// Edit opens the file in the user's editor.
//
// Offered because a settings file is often faster to change in bulk by hand,
// and because hiding the file behind a command would make the two ways of
// editing feel like different systems.
func (s *Schema) Edit() error {
	path := s.Path()
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		return fmt.Errorf("set $EDITOR to edit this file, or edit it directly:\n  %s", path)
	}
	// Make sure there is something to open: an editor invoked on a missing
	// file gives an empty buffer with no hint of the format.
	if _, err := os.Stat(path); err != nil {
		doc, lerr := s.Load()
		if lerr == nil {
			s.Save(doc)
		}
	}
	cmd := exec.Command(ed, path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// --- parsers, shared so every schema validates identically ---------------

// ParseBool accepts the spellings people actually type.
func ParseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "y", "on", "1":
		return true, nil
	case "false", "no", "n", "off", "0":
		return false, nil
	}
	return false, fmt.Errorf("expected true or false, got %q", v)
}

// ParseInt accepts a whole number, or "unlimited"/"none" as zero.
func ParseInt(v string) (int, error) {
	v = strings.TrimSpace(v)
	if v == "unlimited" || v == "none" || v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("expected a whole number, got %q", v)
	}
	return n, nil
}

// ParseFloat accepts a decimal, or "unlimited"/"none" as zero.
func ParseFloat(v string) (float64, error) {
	v = strings.TrimSpace(v)
	if v == "unlimited" || v == "none" || v == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("expected a number, got %q", v)
	}
	return f, nil
}

// ParseDuration accepts Go durations plus bare seconds.
func ParseDuration(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "none" {
		return 0, nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d < 0 {
			return 0, fmt.Errorf("a duration cannot be negative")
		}
		return d, nil
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second, nil
	}
	// Days and weeks, which time.ParseDuration does not know. A half-life or
	// an availability window is naturally expressed in days, and refusing
	// "7d" while defaulting to a week is the kind of inconsistency that
	// makes a config surface feel arbitrary.
	if d, ok := parseLongDuration(v); ok {
		if d < 0 {
			return 0, fmt.Errorf("a duration cannot be negative")
		}
		return d, nil
	}
	return 0, fmt.Errorf("expected a duration like 30s, 5m, 2h, 7d or 2w, got %q", v)
}

// parseLongDuration handles a trailing d or w, alone or followed by a
// duration time.ParseDuration understands -- so "7d", "1w" and "1d12h" all
// work.
func parseLongDuration(v string) (time.Duration, bool) {
	var total time.Duration
	rest := v
	matched := false
	for _, unit := range []struct {
		suffix string
		span   time.Duration
	}{{"w", 7 * 24 * time.Hour}, {"d", 24 * time.Hour}} {
		i := strings.Index(rest, unit.suffix)
		if i <= 0 {
			continue
		}
		n, err := strconv.Atoi(rest[:i])
		if err != nil {
			continue
		}
		total += time.Duration(n) * unit.span
		rest = rest[i+len(unit.suffix):]
		matched = true
	}
	if !matched {
		return 0, false
	}
	if rest != "" {
		// A remainder like the "12h" of "1d12h".
		d, err := time.ParseDuration(rest)
		if err != nil {
			return 0, false
		}
		total += d
	}
	return total, true
}

// ParseSizeGB reads 8G, 512M, 2T or a bare number into gibibytes, which is
// the unit the owner policy uses.
func ParseSizeGB(v string) (float64, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "unlimited" || v == "none" {
		return 0, nil
	}
	mult := 1.0
	switch {
	case strings.HasSuffix(v, "T"), strings.HasSuffix(v, "t"):
		mult, v = 1024, v[:len(v)-1]
	case strings.HasSuffix(v, "G"), strings.HasSuffix(v, "g"):
		mult, v = 1, v[:len(v)-1]
	case strings.HasSuffix(v, "M"), strings.HasSuffix(v, "m"):
		mult, v = 1.0/1024, v[:len(v)-1]
	case strings.HasSuffix(v, "K"), strings.HasSuffix(v, "k"):
		mult, v = 1.0/(1024*1024), v[:len(v)-1]
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("expected a size like 200G, 512M or 'unlimited', got %q", v)
	}
	return f * mult, nil
}

// ParseList splits a comma-separated value.
func ParseList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ParseEnum checks a value against a fixed set.
func ParseEnum(v string, allowed []string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	for _, a := range allowed {
		if v == a {
			return v, nil
		}
	}
	sorted := append([]string(nil), allowed...)
	sort.Strings(sorted)
	return "", fmt.Errorf("expected one of %s, got %q", strings.Join(sorted, ", "), v)
}

// FormatSizeGB renders a gibibyte figure compactly.
func FormatSizeGB(g float64) string {
	switch {
	case g <= 0:
		return "unlimited"
	case g >= 1024:
		return fmt.Sprintf("%.1fT", g/1024)
	case g >= 1:
		return fmt.Sprintf("%gG", g)
	default:
		return fmt.Sprintf("%.0fM", g*1024)
	}
}

// FormatInt renders a count, showing zero as unlimited where that is what it
// means to the field.
func FormatInt(n int, zeroIsUnlimited bool) string {
	if n == 0 && zeroIsUnlimited {
		return "unlimited"
	}
	return strconv.Itoa(n)
}

// editDistance is Levenshtein, for the did-you-mean suggestion.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
