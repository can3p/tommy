package ingress

import (
	"fmt"
	"regexp"
	"strings"
)

// Pattern is a parsed net/http ServeMux pattern: "[METHOD ][HOST]/[PATH]".
type Pattern struct {
	Method string
	Host   string
	Path   string
	Raw    string
}

var methodRe = regexp.MustCompile(`^[A-Z]+$`)

// ParsePattern splits a ServeMux pattern into its parts.
func ParsePattern(pattern string) (Pattern, error) {
	p := Pattern{Raw: pattern}
	rest := strings.TrimSpace(pattern)
	if rest == "" {
		return p, fmt.Errorf("empty pattern")
	}
	if i := strings.IndexAny(rest, " \t"); i >= 0 {
		method := rest[:i]
		if !methodRe.MatchString(method) {
			return p, fmt.Errorf("invalid method %q", method)
		}
		p.Method = method
		rest = strings.TrimSpace(rest[i+1:])
		if rest == "" {
			return p, fmt.Errorf("pattern %q has a method but no path", pattern)
		}
	}
	if i := strings.IndexByte(rest, '/'); i > 0 {
		p.Host = rest[:i]
		rest = rest[i:]
	}
	if !strings.HasPrefix(rest, "/") {
		return p, fmt.Errorf("path must start with /, got %q", rest)
	}
	p.Path = rest
	return p, nil
}

var wildcardRe = regexp.MustCompile(`\{[^{}]*\}`)

// Key returns a comparison key where wildcard names are erased, so
// /a/{id} and /a/{sid} are recognised as the same route.
func (p Pattern) Key() string {
	path := wildcardRe.ReplaceAllStringFunc(p.Path, func(m string) string {
		if strings.HasSuffix(m, "...}") {
			return "{...}"
		}
		if m == "{$}" {
			return "{$}"
		}
		return "{}"
	})
	return p.Host + path
}

// Conflicts reports whether two patterns fight over the same requests.
// Different methods on the same path are fine; a method-less pattern shadows
// every method, so it conflicts with all of them.
func (p Pattern) Conflicts(other Pattern) bool {
	if p.Key() != other.Key() {
		return false
	}
	return p.Method == "" || other.Method == "" || p.Method == other.Method
}

// String renders the pattern back.
func (p Pattern) String() string {
	if p.Method == "" {
		return p.Host + p.Path
	}
	return p.Method + " " + p.Host + p.Path
}

// Concrete turns a pattern path into a request path by filling wildcards with
// sample values, so a mounted route can be probed.
func (p Pattern) Concrete() string {
	path := p.Path
	path = strings.ReplaceAll(path, "{$}", "")
	path = wildcardRe.ReplaceAllStringFunc(path, func(m string) string {
		if strings.HasSuffix(m, "...}") {
			return "probe/probe"
		}
		return "probe"
	})
	if path == "" {
		path = "/"
	}
	return path
}
