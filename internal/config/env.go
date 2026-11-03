package config

import (
	"errors"
	"fmt"
	"strings"
)

// Lookup resolves an environment variable name to its value. ok reports
// whether the variable is set; an empty value with ok true means the variable
// is set but empty. Production code passes os.LookupEnv; tests pass fakes so
// no test touches the real environment.
type Lookup func(name string) (value string, ok bool)

// Expand replaces environment references in s using lookup.
//
// Grammar:
//
//	$$              literal "$"
//	${NAME}         value of NAME; error when NAME is unset or empty
//	${NAME:-def}    value of NAME, or def when NAME is unset or empty
//	$ (otherwise)   literal "$" (references require braces)
//
// NAME must match [A-Za-z0-9_]+. A "${" without a matching "}" is an
// unterminated reference error. Nesting is not supported: a ${ appearing
// inside a reference is rejected with a ConfigError instead of being copied
// through silently. Everything else is ordinary data — in particular a "}"
// outside a reference, before or after one, is copied through unchanged, so
// values like "x${V}y}" and `{"a": "${V}"}` expand and round-trip exactly.
// Defaults are not expanded recursively, but they unescape $$ to $ like the
// rest of the grammar, so ${X:-$$} yields a single "$" when X is unset.
// Expansion is a single pass, so a variable's value is never re-expanded.
// To spell reference-looking text literally, escape the leading dollar:
// $${A:-$$} expands to the literal ${A:-$} with no lookup performed, since
// the $$ consumed first leaves "{A:-$}" as ordinary data.
func Expand(s string, lookup Lookup) (string, error) {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		if i+1 < len(s) && s[i+1] == '{' {
			rest := s[i+2:]
			end := strings.IndexByte(rest, '}')
			if end < 0 {
				if strings.Contains(rest, "${") {
					return "", errNestedReference
				}
				return "", unterminated(rest)
			}
			inner := rest[:end]
			if strings.Contains(inner, "${") {
				return "", errNestedReference
			}
			name, def, hasDefault := strings.Cut(inner, ":-")
			if err := checkName(name); err != nil {
				return "", err
			}
			value, ok := lookup(name)
			switch {
			case ok && value != "":
				b.WriteString(value)
			case hasDefault:
				b.WriteString(unescapeDollars(def))
			case !ok:
				return "", fmt.Errorf("variable %q is not set", name)
			default:
				return "", fmt.Errorf("variable %q is empty", name)
			}
			i += 2 + end + 1
			continue
		}
		b.WriteByte('$')
		i++
	}
	return b.String(), nil
}

// unescapeDollars rewrites each $$ in a default value to $, so a literal
// dollar is spelled uniformly across the grammar.
func unescapeDollars(s string) string {
	if !strings.Contains(s, "$$") {
		return s
	}
	return strings.ReplaceAll(s, "$$", "$")
}

// errNestedReference reports a nested or unbalanced ${...} construction.
// Nesting is not supported; copying fragments of it through would corrupt
// the value silently, so the whole reference is rejected.
var errNestedReference = ConfigError{Msg: "nested ${...} references are not supported"}

// unterminated reports a "${" without a closing "}", naming the variable it
// found before the end of input.
func unterminated(rest string) error {
	name, _, _ := strings.Cut(rest, ":-")
	if name == "" {
		return errors.New(`unterminated variable reference (empty name)`)
	}
	return fmt.Errorf("unterminated variable reference %q", name)
}

// checkName rejects empty or malformed variable names.
func checkName(name string) error {
	if name == "" {
		return errors.New("empty variable name in ${}")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_':
		default:
			return fmt.Errorf("invalid variable name %q", name)
		}
	}
	return nil
}
