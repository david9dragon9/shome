package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/davidwu/shome/internal/config"
	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/fairshare"
)

// `shome admin priority` sets how the queue is ordered.
//
// Goes through the API rather than writing priority.yaml directly, so the
// CLI, the console and a hand-edited file all pass the same validation --
// and so it works from a login session, where the file is not reachable.
// Same arrangement as `shome admin qos`.

func priorityUsage() {
	fmt.Print(`shome admin priority - how the queue is ordered

  priority                       what the policy is now
  priority on | off              turn priority ordering on or off
  priority set FIELD VALUE       change one setting
  priority shares USER N         set an account's entitlement (0 = default)
  priority shares                every account's entitlement
  priority file                  where the policy is stored
  priority explain               what the current policy does, in words

Settings:

`)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, f := range fairshare.Fields() {
		fmt.Fprintf(w, "  %s\t%s\t%s\n", f.Name, f.Kind, f.Help)
	}
	w.Flush()
	fmt.Print(`
Weights are relative: doubling all of them changes nothing.

  shome admin priority on
  shome admin priority set weight-fairshare 2
  shome admin priority set half-life 3d
  shome admin priority shares alice 2

Off by default. With one user, submission order is the right answer and
this changes nothing.
`)
}

func adminPriority(args []string) error {
	if len(args) == 0 {
		return priorityShow()
	}
	switch args[0] {
	case "help", "-h", "--help":
		priorityUsage()
		return nil
	case "show":
		return priorityShow()
	case "explain":
		var v ctl.PriorityView
		if err := call("GET", "/priority", nil, &v); err != nil {
			return err
		}
		fmt.Println(v.Explain)
		return nil
	case "file":
		var v ctl.PriorityView
		if err := call("GET", "/priority", nil, &v); err != nil {
			return err
		}
		fmt.Println(v.File)
		return nil
	case "on", "enable":
		return prioritySet("enabled", "true")
	case "off", "disable":
		return prioritySet("enabled", "false")
	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: shome admin priority set FIELD VALUE\n\n" +
				"See 'shome admin priority help' for the fields")
		}
		return prioritySet(args[1], strings.Join(args[2:], " "))
	case "shares":
		if len(args) == 1 {
			return priorityShares()
		}
		if len(args) < 3 {
			return fmt.Errorf("usage: shome admin priority shares USER N\n\n" +
				"  shome admin priority shares alice 2   twice the default entitlement\n" +
				"  shome admin priority shares alice 0   back to the default")
		}
		n, err := strconv.ParseFloat(args[2], 64)
		if err != nil {
			return fmt.Errorf("%q is not a number", args[2])
		}
		var s fairshare.Share
		if err := call("POST", "/users/"+urlEscape(args[1])+"/shares",
			map[string]any{"shares": n}, &s); err != nil {
			return err
		}
		if n <= 0 {
			fmt.Printf("%s is back to the default entitlement (%s shares)\n",
				args[1], trimNum(s.Shares))
		} else {
			fmt.Printf("%s now has %s shares\n", args[1], trimNum(s.Shares))
		}
		return nil
	}
	priorityUsage()
	return fmt.Errorf("unknown: shome admin priority %s", args[0])
}

func priorityShow() error {
	var v ctl.PriorityView
	if err := call("GET", "/priority", nil, &v); err != nil {
		return err
	}
	c := v.Config
	state := "off"
	if c.Enabled {
		state = "on"
	}
	fmt.Printf("priority ordering: %s\n\n", state)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SETTING\tVALUE")
	rows := [][2]string{
		{"enabled", fmt.Sprint(c.Enabled)},
		{"backfill", fmt.Sprint(c.Backfill)},
		{"backfill-depth", strconv.Itoa(c.BackfillDepth)},
		{"weight-fairshare", trimNum(c.Weights.FairShare)},
		{"weight-age", trimNum(c.Weights.Age)},
		{"weight-size", trimNum(c.Weights.Size)},
		{"favor-small", fmt.Sprint(c.FavorSmall)},
		{"half-life", c.HalfLife.String()},
		{"max-age", c.MaxAge.String()},
		{"gpu-weight", trimNum(c.Resource.GPU)},
		{"mem-gb-weight", trimNum(c.Resource.MemGB)},
		{"default-shares", trimNum(c.DefaultShares)},
	}
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\n", r[0], r[1])
	}
	w.Flush()

	if len(v.Shares) > 0 {
		fmt.Printf("\nstandings:\n\n")
		sw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(sw, "ACCOUNT\tSHARES\tRAW USAGE\tFACTOR")
		for _, s := range v.Shares {
			fmt.Fprintf(sw, "%s\t%s\t%s\t%.4f\n",
				s.User, trimNum(s.Shares), resourceSeconds(s.RawUsage), s.Factor)
		}
		sw.Flush()
	}
	fmt.Printf("\nfile: %s\n%s\n", v.File, v.Explain)
	return nil
}

func priorityShares() error {
	var v ctl.PriorityView
	if err := call("GET", "/priority", nil, &v); err != nil {
		return err
	}
	if len(v.Shares) == 0 {
		fmt.Println("no accounts yet")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ACCOUNT\tSHARES\tSOURCE")
	for _, s := range v.Shares {
		src := "default"
		if _, ok := v.Config.Shares[s.User]; ok {
			src = "set"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", s.User, trimNum(s.Shares), src)
	}
	w.Flush()
	fmt.Printf("\nAn account with twice the shares is entitled to twice the cluster\n")
	fmt.Printf("over time. Set one with: shome admin priority shares USER N\n")
	return nil
}

// prioritySet changes one field.
//
// The whole document is fetched, one field changed and the whole document
// sent back. Field-by-field patching would need every setting to distinguish
// "not sent" from "sent as zero", and for a set of weights that is precisely
// the distinction that matters -- zero is a meaningful weight.
func prioritySet(field, value string) error {
	var v ctl.PriorityView
	if err := call("GET", "/priority", nil, &v); err != nil {
		return err
	}
	c := v.Config
	if err := applyPriorityField(&c, field, value); err != nil {
		return err
	}
	var out ctl.PriorityView
	if err := call("POST", "/priority", c, &out); err != nil {
		return err
	}
	fmt.Printf("%s = %s\n\n%s\n", field, value, out.Explain)
	return nil
}

// applyPriorityField parses one setting into the policy.
//
// The parsers are the shared ones from internal/config, so "3d", "1h30m",
// "yes" and "on" mean here exactly what they mean in every other shome
// config command.
func applyPriorityField(c *fairshare.Config, field, value string) error {
	boolean := func(t *bool) error {
		b, err := config.ParseBool(value)
		if err != nil {
			return err
		}
		*t = b
		return nil
	}
	number := func(t *float64) error {
		f, err := config.ParseFloat(value)
		if err != nil {
			return err
		}
		if f < 0 {
			return fmt.Errorf("%s cannot be negative", field)
		}
		*t = f
		return nil
	}
	span := func(t *time.Duration) error {
		d, err := config.ParseDuration(value)
		if err != nil {
			return err
		}
		if d <= 0 {
			return fmt.Errorf("%s must be a positive duration, e.g. 7d or 12h", field)
		}
		*t = d
		return nil
	}

	switch field {
	case "enabled":
		return boolean(&c.Enabled)
	case "backfill":
		return boolean(&c.Backfill)
	case "favor-small", "favor_small":
		return boolean(&c.FavorSmall)
	case "backfill-depth", "backfill_depth":
		n, err := config.ParseInt(value)
		if err != nil {
			return err
		}
		if n < 1 {
			return fmt.Errorf("backfill-depth must be at least 1")
		}
		c.BackfillDepth = n
		return nil
	case "weight-fairshare", "weight_fairshare", "fairshare":
		return number(&c.Weights.FairShare)
	case "weight-age", "weight_age", "age":
		return number(&c.Weights.Age)
	case "weight-size", "weight_size", "size":
		return number(&c.Weights.Size)
	case "gpu-weight", "gpu_weight":
		return number(&c.Resource.GPU)
	case "mem-gb-weight", "mem_gb_weight":
		return number(&c.Resource.MemGB)
	case "default-shares", "default_shares":
		return number(&c.DefaultShares)
	case "half-life", "half_life":
		return span(&c.HalfLife)
	case "max-age", "max_age":
		return span(&c.MaxAge)
	}

	// An unknown field gets the list rather than a bare refusal: the names
	// are not guessable and a typo should not send anyone to the docs.
	var names []string
	for _, f := range fairshare.Fields() {
		names = append(names, f.Name)
	}
	return fmt.Errorf("unknown setting %q\n\nSettings: %s",
		field, strings.Join(names, ", "))
}
