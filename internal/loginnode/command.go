package loginnode

import (
	"fmt"
	"strings"
)

// splitCommand turns a command line into an argument vector.
//
// It handles quoting and nothing else. There is no variable expansion, no
// globbing, no pipes, no redirection and no substitution -- and characters
// that would mean any of those are refused rather than escaped.
//
// Refusing is deliberate. Nothing downstream interprets a metacharacter: the
// arguments go into an argv array, never through a shell. But a login node is
// exactly where somebody will one day add a convenience that does interpret
// them, and a parser that has already discarded the distinction gives that
// change nowhere to fail safely. Rejecting up front means the guarantee is
// enforced at the boundary rather than assumed downstream.
func splitCommand(line string) ([]string, error) {
	var out []string
	var cur strings.Builder
	var quote rune
	started := false
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			started = true
		case r == ' ' || r == '\t':
			if started || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				started = false
			}
		case strings.ContainsRune("|&;<>$`(){}*?!\\\n\r", r):
			return nil, fmt.Errorf("character %q is not allowed in commands here", string(r))
		default:
			cur.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unclosed quote")
	}
	if started || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out, nil
}
