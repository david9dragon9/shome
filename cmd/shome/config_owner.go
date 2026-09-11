package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/davidwu/shome/internal/config"
	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/owner"
)

// The machine owner's settings: what this computer will give the cluster.
//
// Backed by the same node.yaml the owner can edit by hand, and deliberately
// so -- the file is the interface, and this is a way of editing it that
// validates as it goes. A command that wrote somewhere else would mean two
// sources of truth and a question about which wins.

func ownerConfigSchema() *config.Schema {
	path := func() string { return filepath.Join(ctl.DefaultRoot(), owner.PolicyFile) }

	load := func() (any, error) {
		p := &owner.Policy{}
		b, err := os.ReadFile(path())
		if err != nil {
			if os.IsNotExist(err) {
				return p, nil // a first run has no file yet
			}
			return nil, err
		}
		if err := yaml.Unmarshal(b, p); err != nil {
			// Refusing to edit a file that does not parse is deliberate: a
			// read-modify-write over a broken document would discard whatever
			// the owner was in the middle of writing.
			return nil, fmt.Errorf("%s does not parse, so it cannot be edited safely:\n  %w\n\n"+
				"Fix it by hand, or delete it to start over", path(), err)
		}
		return p, nil
	}

	save := func(doc any) error {
		p := doc.(*owner.Policy)
		b, err := yaml.Marshal(p)
		if err != nil {
			return err
		}
		header := "# This machine's contract with the cluster.\n" +
			"#\n" +
			"# You own this file. A cluster admin can drain your node but cannot\n" +
			"# override what you set here. Edits take effect within seconds.\n" +
			"#\n" +
			"# Change it with 'shome config set NAME VALUE', or edit it directly.\n\n"
		if err := os.MkdirAll(filepath.Dir(path()), 0o700); err != nil {
			return err
		}
		return os.WriteFile(path(), append([]byte(header), b...), 0o600)
	}

	pol := func(doc any) *owner.Policy { return doc.(*owner.Policy) }

	return &config.Schema{
		Title:   "what this computer gives the cluster",
		Command: "shome config",
		Path:    path,
		Load:    load,
		Save:    save,
		Note: "A cluster admin cannot override these. 'shome status' shows what is\n" +
			"happening now; 'shome pause' stops everything immediately.",
		Fields: []config.Field{
			{
				Name: "contribute.max_cores", Kind: config.Int,
				Help: "never use more than this many cores",
				Get: func(d any) (string, bool) {
					v := pol(d).Contribute.MaxCores
					return config.FormatInt(v, true), v != 0
				},
				Set: func(d any, s string) error {
					n, err := config.ParseInt(s)
					if err != nil {
						return err
					}
					pol(d).Contribute.MaxCores = n
					return nil
				},
				Unset: func(d any) { pol(d).Contribute.MaxCores = 0 },
			},
			{
				Name: "contribute.max_mem_gb", Kind: config.Size,
				Help: "never use more memory than this",
				Get: func(d any) (string, bool) {
					v := pol(d).Contribute.MaxMemGB
					return config.FormatSizeGB(v), v != 0
				},
				Set: func(d any, s string) error {
					g, err := config.ParseSizeGB(s)
					if err != nil {
						return err
					}
					pol(d).Contribute.MaxMemGB = g
					return nil
				},
				Unset: func(d any) { pol(d).Contribute.MaxMemGB = 0 },
			},
			{
				Name: "contribute.max_disk_gb", Kind: config.Size,
				Help: "total shome may occupy on this machine",
				Get: func(d any) (string, bool) {
					v := pol(d).Contribute.MaxDiskGB
					return config.FormatSizeGB(v), v != 0
				},
				Set: func(d any, s string) error {
					g, err := config.ParseSizeGB(s)
					if err != nil {
						return err
					}
					pol(d).Contribute.MaxDiskGB = g
					return nil
				},
				Unset: func(d any) { pol(d).Contribute.MaxDiskGB = 0 },
			},
			{
				Name: "contribute.max_inodes", Kind: config.Int,
				Help: "files and directories shome may create",
				Get: func(d any) (string, bool) {
					v := pol(d).Contribute.MaxInodes
					return config.FormatInt(int(v), true), v != 0
				},
				Set: func(d any, s string) error {
					n, err := config.ParseInt(s)
					if err != nil {
						return err
					}
					pol(d).Contribute.MaxInodes = int64(n)
					return nil
				},
				Unset: func(d any) { pol(d).Contribute.MaxInodes = 0 },
			},
			{
				Name: "contribute.min_free_disk_gb", Kind: config.Size,
				Help: "always leave this much free, whatever is using it",
				Get: func(d any) (string, bool) {
					v := pol(d).Contribute.MinFreeDiskGB
					if v == 0 {
						return "no floor", false
					}
					return config.FormatSizeGB(v), true
				},
				Set: func(d any, s string) error {
					g, err := config.ParseSizeGB(s)
					if err != nil {
						return err
					}
					pol(d).Contribute.MinFreeDiskGB = g
					return nil
				},
				Unset: func(d any) { pol(d).Contribute.MinFreeDiskGB = 0 },
			},
			{
				Name: "contribute.gpu", Kind: config.Enum,
				Values: []string{"exclusive", "shared", "never"},
				Help:   "how the cluster may use this machine's GPU",
				Get: func(d any) (string, bool) {
					v := pol(d).Contribute.GPU
					if v == "" {
						return "exclusive", false
					}
					return v, true
				},
				Set: func(d any, s string) error {
					v, err := config.ParseEnum(s, []string{"exclusive", "shared", "never"})
					if err != nil {
						return err
					}
					pol(d).Contribute.GPU = v
					return nil
				},
				Unset: func(d any) { pol(d).Contribute.GPU = "" },
			},
			{
				Name: "availability.schedule", Kind: config.List,
				Help: "when work may run, e.g. \"Mon-Fri 22:00-08:00,Sat-Sun *\"",
				Get: func(d any) (string, bool) {
					v := pol(d).Availability.Schedule
					if len(v) == 0 {
						return "any time", false
					}
					return strings.Join(v, ", "), true
				},
				Set: func(d any, s string) error {
					pol(d).Availability.Schedule = config.ParseList(s)
					return nil
				},
				Unset: func(d any) { pol(d).Availability.Schedule = nil },
			},
			{
				Name: "availability.require", Kind: config.List,
				Help: "conditions required to run: ac_power, screen_locked",
				Get: func(d any) (string, bool) {
					v := pol(d).Availability.Require
					if len(v) == 0 {
						return "nothing", false
					}
					return strings.Join(v, ", "), true
				},
				Set: func(d any, s string) error {
					list := config.ParseList(s)
					for _, r := range list {
						if _, err := config.ParseEnum(r, []string{"ac_power", "screen_locked"}); err != nil {
							return err
						}
					}
					pol(d).Availability.Require = list
					return nil
				},
				Unset: func(d any) { pol(d).Availability.Require = nil },
			},
		},
	}
}

// ownerConfig is `shome config`.
func ownerConfig(args []string) error { return ownerConfigSchema().Run(args) }
