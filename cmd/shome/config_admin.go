package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/davidwu/shome/internal/clustercfg"
	"github.com/davidwu/shome/internal/config"
	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/daemon"
)

// The cluster's own settings: the addresses it listens on and what it serves.
//
// Same engine as the owner's config, a different schema. These were previously
// only settable as flags on `shome up`, which meant the way to change one was
// to remember the whole command line -- and getting it wrong silently dropped
// whatever else had been passed the first time.
//
// Distinct from `shome admin qos`, which is about what accounts may use. This
// is about how the cluster is reachable.

// adminDoc is what `shome admin config` edits.
//
// Two files, one command. The listening sockets live in node.json and the
// cluster's name lives in cluster.yaml -- different files because they have
// different audiences (one is machine state, the other is meant to be
// hand-edited) -- but "what is this cluster called" and "what port does it
// listen on" are both cluster settings, and making somebody learn two
// commands to discover that is the kind of seam users should never see.
type adminDoc struct {
	cfg     *daemon.SavedConfig
	cluster *clustercfg.Config
}

func adminConfigSchema() *config.Schema {
	root := func() string { return ctl.DefaultRoot() }

	load := func() (any, error) {
		cfg, ok := daemon.LoadConfig(root())
		if !ok {
			return nil, fmt.Errorf("this machine is not set up as a cluster yet.\n" +
				"Start one with 'shome up', or join one with 'shome join'")
		}
		if cfg.Role != daemon.RoleController {
			return nil, fmt.Errorf("cluster settings live on the controller, and this\n" +
				"machine is a node. Run this there instead")
		}
		cl, err := clustercfg.Load(root())
		if err != nil {
			return nil, err
		}
		return &adminDoc{cfg: &cfg, cluster: &cl}, nil
	}

	save := func(doc any) error {
		d := doc.(*adminDoc)
		daemon.SaveConfig(root(), *d.cfg)
		return clustercfg.Save(root(), *d.cluster)
	}

	c := func(doc any) *daemon.SavedConfig { return doc.(*adminDoc).cfg }
	cl := func(doc any) *clustercfg.Config { return doc.(*adminDoc).cluster }
	restart := "Takes effect at the next 'shome restart'."

	// Addresses are validated the same way in three places, so the check
	// lives once. An unparseable listen address would otherwise be accepted
	// here and fail at startup, where it reads as a broken install.
	setAddr := func(target *string, defPort int, allowEmpty bool) func(any, string) error {
		return func(_ any, s string) error {
			s = strings.TrimSpace(s)
			if s == "" || s == "off" || s == "none" {
				if !allowEmpty {
					return fmt.Errorf("this address is required")
				}
				*target = ""
				return nil
			}
			// A bare port or a bare host are both things people type.
			if !strings.Contains(s, ":") {
				if n, err := strconv.Atoi(s); err == nil {
					s = net.JoinHostPort("0.0.0.0", strconv.Itoa(n))
				} else {
					s = net.JoinHostPort(s, strconv.Itoa(defPort))
				}
			}
			host, port, err := net.SplitHostPort(s)
			if err != nil {
				return fmt.Errorf("expected host:port, a bare port, or 'off', got %q", s)
			}
			if _, err := strconv.Atoi(port); err != nil {
				return fmt.Errorf("%q is not a port number", port)
			}
			if host == "" {
				host = "0.0.0.0"
			}
			*target = net.JoinHostPort(host, port)
			return nil
		}
	}

	return &config.Schema{
		Title:   "how this cluster is reachable",
		Command: "shome admin config",
		Path:    func() string { return root() + "/node.json" },
		Load:    load,
		Save:    save,
		Note: "Account limits are elsewhere: 'shome admin qos'.\n" +
			"Most of these need a restart, because they are listening sockets.",
		Fields: []config.Field{
			{
				Name: "cluster", Kind: config.String,
				Help: "what this cluster is called",
				Effect: "Takes effect immediately: shell prompts, the login banner and\n" +
					"the console title all read it live.",
				Get: func(d any) (string, bool) {
					v := cl(d).Name
					return cl(d).ClusterName(), v != ""
				},
				Set: func(d any, s string) error {
					s = strings.TrimSpace(s)
					if err := clustercfg.ValidName(s); err != nil {
						return err
					}
					cl(d).Name = s
					return nil
				},
				Unset: func(d any) { cl(d).Name = "" },
			},
			{
				Name: "node", Kind: config.String,
				Help: "this machine's own identity in the cluster",
				Effect: restart + "\n" +
					"Changing it re-joins this machine under a new identity. To rename\n" +
					"a machine for display, use 'shome admin node label' instead --\n" +
					"that needs no restart and keeps its certificate and job history.",
				Get: func(d any) (string, bool) { return c(d).Node, c(d).Node != "" },
				Set: func(d any, s string) error {
					s = strings.TrimSpace(s)
					if s == "" {
						return fmt.Errorf("a node needs a name")
					}
					if strings.ContainsAny(s, "/ \t,") {
						// The name becomes a path component in scratch
						// directories and a field in every listing.
						return fmt.Errorf("a node name cannot contain spaces, commas or slashes")
					}
					c(d).Node = s
					return nil
				},
			},
			{
				Name: "listen", Kind: config.String,
				Help:   "address node agents connect to",
				Effect: restart,
				Get: func(d any) (string, bool) {
					v := c(d).Listen
					if v == "" {
						return fmt.Sprintf("0.0.0.0:%d", daemon.DefaultPort), false
					}
					return v, true
				},
				Set: func(d any, s string) error {
					return setAddr(&c(d).Listen, daemon.DefaultPort, false)(d, s)
				},
				Unset: func(d any) { c(d).Listen = "" },
			},
			{
				Name: "web", Kind: config.String,
				Help:   "admin console address, or 'off'",
				Effect: restart,
				Get: func(d any) (string, bool) {
					v := c(d).WebAddr
					if v == "" {
						return "off", true
					}
					return v, true
				},
				Set: func(d any, s string) error {
					if err := setAddr(&c(d).WebAddr, 7820, true)(d, s); err != nil {
						return err
					}
					// Worth saying every time. The console has no login of its
					// own beyond a bearer token, so binding it off loopback
					// hands the cluster to the LAN.
					if a := c(d).WebAddr; a != "" && !strings.HasPrefix(a, "127.0.0.1:") &&
						!strings.HasPrefix(a, "localhost:") {
						fmt.Printf("\nwarning: %s is reachable from the network. The console\n"+
							"authenticates with a bearer token and nothing else; prefer\n"+
							"loopback plus an ssh tunnel.\n", a)
					}
					return nil
				},
				Unset: func(d any) { c(d).WebAddr = "" },
			},
			{
				Name: "ssh", Kind: config.String,
				Help:   "login node address, or 'off'",
				Effect: restart,
				Get: func(d any) (string, bool) {
					v := c(d).SSHAddr
					if v == "" {
						return "off", true
					}
					return v, true
				},
				Set:   func(d any, s string) error { return setAddr(&c(d).SSHAddr, 2222, true)(d, s) },
				Unset: func(d any) { c(d).SSHAddr = "" },
			},
			{
				Name: "monitor", Kind: config.String,
				Help:   "the machine owner's local page, or 'off' (loopback only)",
				Effect: restart,
				Get: func(d any) (string, bool) {
					v := c(d).MonitorAddr
					if v == "" {
						return "off", true
					}
					return v, true
				},
				Set: func(d any, s string) error {
					if err := setAddr(&c(d).MonitorAddr, 7821, true)(d, s); err != nil {
						return err
					}
					// Refused rather than warned about, unlike the console:
					// this page has no credential at all, and pausing the
					// machine through it needs none.
					if a := c(d).MonitorAddr; a != "" {
						host, _, _ := net.SplitHostPort(a)
						if host != "127.0.0.1" && host != "localhost" && host != "::1" {
							c(d).MonitorAddr = ""
							return fmt.Errorf("the owner monitor may only listen on loopback.\n" +
								"It has no login: it shows this machine's state and can pause it,\n" +
								"which is for whoever is sitting here. Use an ssh tunnel instead")
						}
					}
					return nil
				},
				Unset: func(d any) { c(d).MonitorAddr = "" },
			},
			{
				Name: "advertise", Kind: config.List,
				Help: "extra addresses agents should dial, e.g. a VPN address",
				Effect: "Takes effect at the next 'shome restart', which also reissues\n" +
					"the certificate so the new address is covered.",
				Get: func(d any) (string, bool) {
					v := c(d).Advertise
					if v == "" {
						return "whatever this machine's interfaces say", false
					}
					return v, true
				},
				Set: func(d any, s string) error {
					c(d).Advertise = strings.Join(config.ParseList(s), ",")
					return nil
				},
				Unset: func(d any) { c(d).Advertise = "" },
			},
		},
	}
}

// adminConfig is `shome admin config`.
func adminConfig(args []string) error { return adminConfigSchema().Run(args) }
