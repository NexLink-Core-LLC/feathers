//go:build windows

package winproc

import (
	"regexp"
	"strings"

	"emperror.dev/errors"
)

// placeholderRegex matches a Pterodactyl startup placeholder such as
// {{SERVER_MEMORY}} or {{ SERVER_PORT }} (surrounding whitespace tolerated).
var placeholderRegex = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// envMap converts a slice of KEY=VALUE strings into a lookup map. The first '='
// separates key from value; values may themselves contain '='.
func envMap(vars []string) map[string]string {
	m := make(map[string]string, len(vars))
	for _, kv := range vars {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

// substitutePlaceholders replaces every {{VAR}} token in s with its value from
// env. Unknown placeholders collapse to an empty string, mirroring the Linux
// container entrypoint, which does the same substitution before exec.
func substitutePlaceholders(s string, env map[string]string) string {
	return placeholderRegex.ReplaceAllStringFunc(s, func(tok string) string {
		m := placeholderRegex.FindStringSubmatch(tok)
		if m == nil {
			return tok
		}
		return env[m[1]]
	})
}

// tokenize splits a command line into an argv slice, honouring single and
// double quotes (the quote characters are removed; whitespace inside them is
// preserved). It deliberately performs NO glob or variable expansion — the
// substituted egg values are treated as opaque data, never re-interpreted by a
// shell. This is what makes argv-form launching safe against the command
// injection that `cmd.exe /c "<interpolated string>"` would invite.
func tokenize(s string) []string {
	var (
		args  []string
		cur   strings.Builder
		inArg bool
		quote rune // 0 when unquoted, otherwise the active quote rune
	)
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inArg = true
		case r == ' ' || r == '\t' || r == '\r' || r == '\n':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args
}

// buildCommand resolves a server's startup invocation into an argv slice. It
// reads the STARTUP variable from the assembled environment, substitutes every
// {{VAR}} placeholder from that same environment, and tokenizes the result.
//
// On Linux this work is done by the Docker image's entrypoint script; there is
// no container on Windows, so winproc replicates it. See
// docs/phase0-conformance-spec.md §1.
func buildCommand(vars []string) ([]string, error) {
	env := envMap(vars)

	startup := env["STARTUP"]
	if strings.TrimSpace(startup) == "" {
		return nil, errors.New("winproc: no STARTUP command is defined for this server")
	}

	argv := tokenize(substitutePlaceholders(startup, env))
	if len(argv) == 0 {
		return nil, errors.New("winproc: startup command resolved to an empty argv")
	}
	return argv, nil
}
