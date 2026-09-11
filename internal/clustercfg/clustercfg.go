// Package clustercfg holds the cluster's own identity: what it is called, and
// what the admin calls each machine in it.
//
// Separate from the QoS limits (what accounts may use) and from node.json
// (which sockets this machine listens on), because this is the only
// cluster-level state a person reads and writes for its own sake. It lives in
// a YAML file the admin may edit directly, and the CLI and web console are
// two more doors onto the same file.
//
// # Labels are not renames
//
// A node's identity is the common name in its mTLS certificate, and the
// controller overwrites whatever name a heartbeat claims with the one on the
// certificate. Changing that identity would mean re-issuing the node's
// certificate and rewriting every job record that refers to it.
//
// So an admin sets a *label*: a name shown wherever a person reads a node
// name, and accepted wherever a person types one. The certificate identity
// underneath is untouched. This is a real distinction rather than a
// limitation dressed up -- it means naming a machine can never invalidate its
// credentials or orphan its job history.
package clustercfg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultName is used when the admin has not chosen one. Deliberately not the
// controller's hostname: the cluster outlives any one machine's name.
const DefaultName = "shome"

// Config is the contents of cluster.yaml.
type Config struct {
	// Name is what the cluster is called. It appears in shell prompts, the
	// console title and the login banner.
	Name string `yaml:"name,omitempty"`

	// Nodes maps a machine's certificate identity to what the admin calls it.
	Nodes map[string]Node `yaml:"nodes,omitempty"`
}

// Node is the admin's view of one machine.
type Node struct {
	// Label is the name shown and accepted in place of the identity.
	Label string `yaml:"label,omitempty"`
	// Note is free text -- "in the study", "flaky wifi" -- shown next to the
	// machine so an admin can leave themselves a reminder.
	Note string `yaml:"note,omitempty"`
}

// File is where the config lives.
func File(root string) string { return filepath.Join(root, "cluster.yaml") }

// ClusterName returns the configured name or the default.
func (c Config) ClusterName() string {
	if strings.TrimSpace(c.Name) == "" {
		return DefaultName
	}
	return c.Name
}

// LabelOf returns what to call a machine: its label if the admin set one,
// otherwise its own identity.
func (c Config) LabelOf(identity string) string {
	if n, ok := c.Nodes[identity]; ok && strings.TrimSpace(n.Label) != "" {
		return n.Label
	}
	return identity
}

// NoteOf returns the admin's note for a machine, if any.
func (c Config) NoteOf(identity string) string { return c.Nodes[identity].Note }

// Resolve turns a name a person typed into a machine's certificate identity.
//
// Accepts the identity itself, a label, or either with differing case, so
// that naming a machine "mini" makes `shome fs ls mini:` work without the
// user needing to know what the certificate says. Returns the input unchanged
// when nothing matches, leaving "no such machine" to the caller that knows
// which machines exist.
func (c Config) Resolve(name string) string {
	if name == "" {
		return name
	}
	if _, ok := c.Nodes[name]; ok {
		return name
	}
	for identity, n := range c.Nodes {
		if strings.EqualFold(n.Label, name) || strings.EqualFold(identity, name) {
			return identity
		}
	}
	return name
}

// CanLabel reports whether identity may be called name.
//
// Here rather than in the HTTP handler because it is the rule that keeps
// resolution unambiguous, and a rule enforced only on the path somebody
// happened to test is a rule that the next caller gets wrong. identities is
// every machine the cluster knows.
func (c Config) CanLabel(identity, name string, identities []string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	// Another machine's label. Allowing it would make one name resolve to two
	// machines, with the winner decided by map iteration order.
	if other := c.Resolve(name); other != identity && other != name {
		return fmt.Errorf("%q already refers to %s", name, other)
	}
	for _, id := range identities {
		if id != identity && strings.EqualFold(id, name) {
			return fmt.Errorf("%q is already the identity of another machine", name)
		}
	}
	return nil
}

// NormaliseLabel turns a label into what should be stored.
//
// A label identical to the machine's own identity is stored as none: it says
// nothing, and keeping it would leave a stanza in the file that looks like a
// setting but changes nothing.
func NormaliseLabel(identity, name string) string {
	if strings.EqualFold(strings.TrimSpace(name), identity) {
		return ""
	}
	return strings.TrimSpace(name)
}

// ValidName checks a cluster or machine name.
//
// Both end up in shell prompts, file paths and URLs, so the accepted set is
// narrow on purpose: a name with a space or a slash in it would be a quoting
// bug waiting to be found somewhere downstream.
func ValidName(s string) error {
	if s == "" {
		return fmt.Errorf("a name cannot be empty")
	}
	if len(s) > 40 {
		return fmt.Errorf("a name should be 40 characters or fewer")
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("a name may only contain letters, digits, dot, dash "+
				"and underscore -- %q is not allowed", string(r))
		}
	}
	return nil
}

// Load reads cluster.yaml. A missing file is not an error: an unnamed cluster
// is a perfectly good cluster.
func Load(root string) (Config, error) {
	b, err := os.ReadFile(File(root))
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("%s does not parse: %w", File(root), err)
	}
	return c, nil
}

// Save writes cluster.yaml, replacing it atomically.
func Save(root string, c Config) error {
	// Drop entries that say nothing, so hand-editing the file and clearing a
	// label does not leave a growing list of empty stanzas behind.
	for k, n := range c.Nodes {
		if strings.TrimSpace(n.Label) == "" && strings.TrimSpace(n.Note) == "" {
			delete(c.Nodes, k)
		}
	}
	if len(c.Nodes) == 0 {
		c.Nodes = nil
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	header := "# shome cluster identity. Edit by hand, or use 'shome admin config'\n" +
		"# and 'shome admin node label'. A label is a display name; a machine's\n" +
		"# real identity is the common name in its certificate and is not changed.\n"
	tmp := File(root) + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(header), b...), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, File(root))
}

// Cache re-reads cluster.yaml when it changes on disk, so hand-editing the
// file takes effect without a restart -- the same contract as qos.yaml.
type Cache struct {
	path string

	mu   sync.Mutex
	val  Config
	mod  time.Time
	size int64
	read bool
}

func NewCache(root string) *Cache { return &Cache{path: File(root)} }

// Logger is the subset of slog this package needs.
type Logger interface {
	Warn(msg string, args ...any)
}

// Get returns the current config, reloading if the file changed.
//
// A file that does not parse keeps whatever is already in force, loudly: a
// typo should not silently rename the cluster back to its default in every
// prompt and banner.
func (c *Cache) Get(log Logger) Config {
	c.mu.Lock()
	defer c.mu.Unlock()

	fi, err := os.Stat(c.path)
	if err != nil {
		if !c.read {
			c.val, c.read = Config{}, true
		}
		return c.val
	}
	if c.read && fi.ModTime().Equal(c.mod) && fi.Size() == c.size {
		return c.val
	}
	cfg, err := Load(filepath.Dir(c.path))
	if err != nil {
		if log != nil {
			log.Warn("cluster.yaml does not parse; keeping the names already in force",
				"file", c.path, "err", err)
		}
		if !c.read {
			c.val, c.read = Config{}, true
		}
		return c.val
	}
	c.val, c.mod, c.size, c.read = cfg, fi.ModTime(), fi.Size(), true
	return c.val
}
