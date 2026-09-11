package fairshare

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// The policy file, read and written the same way qos.yaml and cluster.yaml
// are: hand-editable, re-read when it changes, and never the only door --
// the CLI and the console write the same file.

// File is where the priority policy lives.
func File(root string) string { return filepath.Join(root, "priority.yaml") }

// Load reads the policy. A missing file means the default, which is disabled
// -- a cluster that has never been configured must behave as it did before
// this feature existed.
func Load(root string) (Config, error) {
	b, err := os.ReadFile(File(root))
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), nil
		}
		return Default(), err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Default(), fmt.Errorf("%s does not parse: %w", File(root), err)
	}
	return c.WithDefaults(), nil
}

// Save writes the policy atomically.
func Save(root string, c Config) error {
	for u, sh := range c.Shares {
		// A share of zero or less is not an entitlement, it is a mistake or a
		// leftover; dropping it means the account falls back to the default
		// rather than being permanently last.
		if sh <= 0 {
			delete(c.Shares, u)
		}
	}
	if len(c.Shares) == 0 {
		c.Shares = nil
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	header := "# shome priority policy. Edit by hand, or use 'shome admin priority'.\n" +
		"# enabled: false means strict submission order, as if this file were absent.\n" +
		"# Weights are relative -- doubling all of them changes nothing.\n"
	tmp := File(root) + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(header), b...), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, File(root))
}

// Logger is the subset of slog this package needs.
type Logger interface {
	Warn(msg string, args ...any)
}

// Cache re-reads the policy when the file changes, so hand-editing takes
// effect without a restart.
type Cache struct {
	path string

	mu   sync.Mutex
	val  Config
	mod  time.Time
	size int64
	read bool
}

func NewCache(root string) *Cache { return &Cache{path: File(root)} }

// Get returns the policy in force.
//
// A file that does not parse keeps whatever is already loaded, loudly. The
// alternative -- falling back to the default -- would silently turn priority
// off across the cluster because of a typo, which is the wrong direction to
// fail: an admin who enabled fair-share would find FIFO back with no
// indication why.
func (c *Cache) Get(log Logger) Config {
	c.mu.Lock()
	defer c.mu.Unlock()

	fi, err := os.Stat(c.path)
	if err != nil {
		if !c.read {
			c.val, c.read = Default(), true
		}
		return c.val
	}
	if c.read && fi.ModTime().Equal(c.mod) && fi.Size() == c.size {
		return c.val
	}
	cfg, err := Load(filepath.Dir(c.path))
	if err != nil {
		if log != nil {
			log.Warn("priority.yaml does not parse; keeping the policy already in force",
				"file", c.path, "err", err)
		}
		if !c.read {
			c.val, c.read = Default(), true
		}
		return c.val
	}
	c.val, c.mod, c.size, c.read = cfg, fi.ModTime(), fi.Size(), true
	return c.val
}

// Fields describes every setting, for the config command and the console.
//
// One list, so the CLI's table, the console's form and the documentation
// cannot drift apart. Mirrors how qos.Fields works.
type FieldInfo struct {
	Name string
	Help string
	Kind string // "bool", "int", "float", "duration"
}

func Fields() []FieldInfo {
	return []FieldInfo{
		{"enabled", "order the queue by priority instead of submission time", "bool"},
		{"backfill", "let a shorter job use a gap it will hand back in time", "bool"},
		{"backfill-depth", "how many waiting jobs get a machine held for them", "int"},
		{"weight-fairshare", "how much recent usage lowers priority", "float"},
		{"weight-age", "how much waiting raises priority", "float"},
		{"weight-size", "how much job size matters (0 = not at all)", "float"},
		{"favor-small", "with weight-size set, prefer smaller jobs", "bool"},
		{"half-life", "how long until past usage counts half as much", "duration"},
		{"max-age", "the wait at which the age factor is at its maximum", "duration"},
		{"gpu-weight", "GPU-seconds per CPU-second when measuring usage", "float"},
		{"mem-gb-weight", "GB-seconds per CPU-second when measuring usage", "float"},
		{"default-shares", "entitlement for an account with no share set", "float"},
	}
}
