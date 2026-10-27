// Package follow resolves the remote working directory implied by a shell
// command line, so the SFTP browser can track "cd" typed in the terminal.
//
// It is deliberately conservative: whenever a line could move the shell to a
// directory we cannot name with confidence, it reports not-applicable and the
// caller leaves the browser where it is.
package follow

import (
	"path"
	"strings"
)

// commands are the shell builtins we recognise as changing the working
// directory. pushd is included because it also lands on its argument.
var commands = map[string]bool{"cd": true, "pushd": true}

// Resolve reports the directory the remote shell ends up in after running
// line, given it currently sits in cwd and the user's home is home.
//
// It returns ok=false for anything ambiguous: pipelines, redirections,
// parameter expansion, tab-completed lines (the completion text never reaches
// us as input), "cd -", "~user" forms, unterminated quotes or extra arguments.
func Resolve(cwd, home, line string) (next string, ok bool) {
	if strings.ContainsAny(line, "|<>$`") {
		return "", false
	}
	if strings.ContainsRune(line, '\t') {
		return "", false
	}
	if cwd == "" {
		cwd = "/"
	}
	if home == "" {
		home = "/"
	}
	segs := splitSegments(line)
	// The last directory-changing segment wins: "cd /a && cd /b" ends in /b.
	for i := len(segs) - 1; i >= 0; i-- {
		toks, tokOK := tokenize(segs[i])
		if !tokOK {
			return "", false
		}
		if len(toks) == 0 || !commands[toks[0]] {
			continue
		}
		return apply(cwd, home, toks[1:])
	}
	return "", false
}

// splitSegments breaks a command line on the ; and && separators, dropping
// empty pieces. Quote awareness is intentionally left out: a separator inside
// a quoted path is vanishingly rare here and only costs us a missed update.
func splitSegments(line string) []string {
	normalised := strings.ReplaceAll(line, "&&", ";")
	parts := strings.Split(normalised, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		// A trailing "&" only backgrounds the segment; the cd still runs.
		p = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(p), "&"))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// tokenize splits one segment on unquoted whitespace and removes the quotes.
// It reports false when a quote is never closed, because the shell would then
// still be reading the command.
func tokenize(seg string) ([]string, bool) {
	var (
		out   []string
		cur   strings.Builder
		quote rune
	)
	for _, r := range seg {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, false
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out, true
}

// apply turns a cd argument list into an absolute path.
func apply(cwd, home string, args []string) (string, bool) {
	if len(args) > 1 {
		return "", false
	}
	arg := ""
	if len(args) == 1 {
		arg = args[0]
	}
	switch {
	case arg == "" || arg == "~":
		return clean(home), true
	case arg == "-":
		// The previous directory is known only to the caller's history.
		return "", false
	case strings.HasPrefix(arg, "~/"):
		return clean(path.Join(home, arg[2:])), true
	case strings.HasPrefix(arg, "~"):
		// ~user expansion is not something we can resolve over SFTP.
		return "", false
	case path.IsAbs(arg):
		return clean(arg), true
	default:
		return clean(path.Join(cwd, arg)), true
	}
}

func clean(p string) string {
	p = path.Clean(p)
	if p == "" {
		return "/"
	}
	return p
}
