package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/davidwu/shome/internal/fairshare"
)

// `sshare` answers "why is my job behind theirs".
//
// Named after Slurm's command, and reporting the same columns, because the
// concept is Slurm's: inventing a new name and new words for it would help
// nobody who has used a cluster before. It takes Slurm's flags too, so a
// remembered `sshare -a` works.

func sshareUsage() {
	fmt.Print(`sshare - fair-share standing

  sshare                your own standing
  sshare -a, --all      every account (admin)
  sshare -u USER        one account (admin)

  SHARES      the entitlement, relative to everyone else's
  NORM SHARES that entitlement as a fraction of the whole cluster
  RAW USAGE   what has been used recently, in weighted resource-seconds,
              with older use counting for less
  NORM USAGE  usage as a fraction of everyone's
  FACTOR      2^(-usage/shares): 1.00 means nothing used, 0.50 means exactly
              the entitlement, and lower means over it, so those jobs wait
              behind others'

A low factor is not a penalty and nothing is being withheld -- it just means
somebody who has used less goes first when you both have work queued.

Set the policy with 'shome admin priority'.
`)
}

func sshare(args []string) error {
	who := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "help", "-h", "--help":
			sshareUsage()
			return nil
		case "-a", "--all", "-A":
			who = "all"
		case "-u", "--user":
			if i+1 >= len(args) {
				return fmt.Errorf("-u needs an account name")
			}
			i++
			who = args[i]
		case "-U", "--me", "--self":
			who = ""
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown option %q\n\n"+
					"  sshare            your own standing\n"+
					"  sshare -a         every account\n"+
					"  sshare -u USER    one account", args[i])
			}
			// A bare name still works: `sshare alice` and `sshare all` are
			// what people try before reading anything.
			who = args[i]
		}
	}
	var out struct {
		Enabled bool              `json:"enabled"`
		Shares  []fairshare.Share `json:"shares"`
		Explain string            `json:"explain"`
	}
	// No account named leaves the choice to the server, which answers with
	// the caller's own row.
	q := "/share"
	if who != "" {
		q += "?user=" + urlEscape(who)
	}
	if err := call("GET", q, nil, &out); err != nil {
		return err
	}
	if len(out.Shares) == 0 {
		fmt.Println("no accounts to report on")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ACCOUNT\tSHARES\tNORM SHARES\tRAW USAGE\tNORM USAGE\tFACTOR")
	for _, s := range out.Shares {
		fmt.Fprintf(w, "%s\t%s\t%.4f\t%s\t%.4f\t%.4f\n",
			s.User, trimNum(s.Shares), s.NormShares,
			resourceSeconds(s.RawUsage), s.NormUsage, s.Factor)
	}
	w.Flush()

	fmt.Printf("\n%s\n", out.Explain)
	if !out.Enabled {
		// Said plainly: showing a table of factors that nothing acts on
		// would imply the cluster is using them.
		fmt.Printf("\nThese numbers are informational right now -- priority is off, so\n")
		fmt.Printf("jobs run in submission order regardless of them.\n")
	}
	return nil
}

// resourceSeconds renders usage in units a person can hold in their head.
//
// Raw resource-seconds run to seven digits within a day of ordinary use,
// which is unreadable and invites false precision.
func resourceSeconds(v float64) string {
	switch {
	case v <= 0:
		return "0"
	case v < 3600:
		return fmt.Sprintf("%.0fs", v)
	case v < 3600*48:
		return fmt.Sprintf("%.1fh", v/3600)
	default:
		return fmt.Sprintf("%.1fd", v/86400)
	}
}

// trimNum prints a float without a pointless decimal point.
func trimNum(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}
