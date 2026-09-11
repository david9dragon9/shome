package ctl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/davidwu/shome/internal/qos"
	"github.com/davidwu/shome/internal/store"
)

// Where QoS limits live, and how they are resolved for one account.
//
// Cluster defaults are a YAML file in the state directory, re-read when it
// changes. A file rather than a database row because an admin asked to be able
// to edit it directly -- and because limits are policy, which belongs
// somewhere reviewable and diffable rather than inside a database that has to
// be queried to see what the rules are.
//
// Per-account overrides live in the database next to the account, because they
// are per-account facts with the same lifetime as the account, and deleting
// somebody should not leave their limits behind in a file.

// QoSFile is the cluster-wide defaults file.
func QoSFile(root string) string { return filepath.Join(root, "qos.yaml") }

// qosCache re-reads the defaults file when it changes, so an edit takes effect
// without a restart -- the same promise the owner policy file makes.
type qosCache struct {
	path string
	mu   sync.Mutex
	val  qos.Config
	mod  time.Time
	size int64
	read bool
}

func newQoSCache(root string) *qosCache { return &qosCache{path: QoSFile(root)} }

// Config returns the limits, reloading if the file changed.
//
// A file that exists but does not parse is ignored in favour of what is
// already in force, loudly. Falling back to "no limits" on a syntax error
// would turn a typo into an unbounded cluster, which is the wrong direction
// to fail.
func (q *qosCache) Config(log logger) qos.Config {
	q.mu.Lock()
	defer q.mu.Unlock()

	fi, err := os.Stat(q.path)
	if err != nil {
		if !q.read {
			q.val, q.read = qos.DefaultConfig(), true
		}
		return q.val
	}
	if q.read && fi.ModTime().Equal(q.mod) && fi.Size() == q.size {
		return q.val
	}
	b, err := os.ReadFile(q.path)
	if err != nil {
		return q.val
	}

	// Peek at the shape first. A file written before cluster totals existed
	// has the limits at the top level and means them per-account; reading it
	// as the new shape would silently discard every one of them.
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		if log != nil {
			log.Warn("qos.yaml does not parse; keeping the limits already in force",
				"file", q.path, "err", err)
		}
		if !q.read {
			q.val, q.read = qos.DefaultConfig(), true
		}
		return q.val
	}
	var cfg qos.Config
	if (qos.Config{}).LooksLegacy(raw) {
		var flat qos.Limits
		if err := yaml.Unmarshal(b, &flat); err == nil {
			cfg = qos.Config{PerUser: flat}
			if log != nil {
				log.Warn("qos.yaml uses the old flat layout; reading it as per-account limits. "+
					"Run 'shome admin qos set default' once to rewrite it",
					"file", q.path)
			}
		}
	} else if err := yaml.Unmarshal(b, &cfg); err != nil {
		if log != nil {
			log.Warn("qos.yaml does not parse; keeping the limits already in force",
				"file", q.path, "err", err)
		}
		return q.val
	}
	q.val, q.mod, q.size, q.read = cfg, fi.ModTime(), fi.Size(), true
	return q.val
}

// logger is the little of *slog.Logger this needs.
type logger interface {
	Warn(msg string, args ...any)
}

// QoSConfig returns both layers of limits.
func (c *Controller) QoSConfig() qos.Config { return c.qos.Config(c.log) }

// QoSDefaults returns the limits applied to each account.
func (c *Controller) QoSDefaults() qos.Limits { return c.QoSConfig().PerUser }

// QoSCluster returns the aggregate ceiling across everybody.
func (c *Controller) QoSCluster() qos.Limits { return c.QoSConfig().Cluster }

// SetQoSConfig writes both layers.
func (c *Controller) SetQoSConfig(cfg qos.Config) error {
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	header := "# shome QoS limits.\n" +
		"#\n" +
		"# Two separate things:\n" +
		"#\n" +
		"#   cluster:   totals across everybody. What this cluster will hand out\n" +
		"#              at once, whoever is asking. Omit to leave unlimited.\n" +
		"#   per_user:  what each account gets, unless overridden for one with\n" +
		"#              'shome admin qos set NAME --...'.\n" +
		"#\n" +
		"# An omitted limit is unlimited. A limit of 0 means none allowed.\n" +
		"# Edits take effect within seconds; no restart.\n\n"
	if err := os.WriteFile(QoSFile(c.root), append([]byte(header), b...), 0o600); err != nil {
		return err
	}
	c.log.Info("QoS limits updated")
	return nil
}

// SetQoSDefaults replaces just the per-account layer.
func (c *Controller) SetQoSDefaults(l qos.Limits) error {
	cfg := c.QoSConfig()
	cfg.PerUser = l
	return c.SetQoSConfig(cfg)
}

// SetQoSCluster replaces just the aggregate layer.
func (c *Controller) SetQoSCluster(l qos.Limits) error {
	cfg := c.QoSConfig()
	cfg.Cluster = l
	return c.SetQoSConfig(cfg)
}

// UserQoS returns an account's overrides.
func (c *Controller) UserQoS(ctx context.Context, name string) (qos.Limits, error) {
	raw, err := c.store.UserQoS(ctx, name)
	if err != nil {
		return qos.Limits{}, err
	}
	var l qos.Limits
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			return qos.Limits{}, fmt.Errorf("stored limits for %s are corrupt: %w", name, err)
		}
	}
	return l, nil
}

// SetUserQoS replaces an account's overrides.
func (c *Controller) SetUserQoS(ctx context.Context, name string, l qos.Limits) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if err := c.store.SetUserQoS(ctx, name, string(b)); err != nil {
		return err
	}
	c.store.Event(ctx, 0, "qos_user_set", name, c.now())
	return nil
}

// EffectiveQoS resolves what actually applies to an account.
func (c *Controller) EffectiveQoS(ctx context.Context, name string) (qos.Effective, error) {
	user, err := c.UserQoS(ctx, name)
	if err != nil {
		return qos.Effective{}, err
	}
	return c.QoSConfig().ResolveFor(user), nil
}

// qosDiskBytes is the account's disk limit in bytes, for reporting.
func qosDiskBytes(a *API, r *http.Request, u *store.User) int64 {
	e, err := a.c.EffectiveQoS(r.Context(), u.Name)
	if err != nil || e.MaxDiskMB < 0 {
		return 0
	}
	return e.MaxDiskMB << 20
}
