package hl7

import (
	"strconv"
	"strings"
)

// Value resolves an HL7 path against the message, returning "" when nothing is
// there. It is the readable way to reach into a parsed message, and it is what
// a provider building an acknowledgement or a test asserting on a fixture
// should use rather than indexing slices by hand.
//
// The grammar is the one people already write in HL7 specifications and
// interface documents:
//
//	MSH-9          the whole field, repetitions joined
//	MSH-9.1        the first component of the first repetition
//	PID-5.1.2      a subcomponent
//	PID-3[2]       the second repetition of a repeating field
//	PID-3[2].1     a component of it
//	OBX(3)-5       the third OBX segment in the message
//
// A missing occurrence, repetition, component or subcomponent index is 1, which
// is what makes MSH-9.1 mean what everyone expects.
func (m *Message) Value(path string) string {
	if m == nil {
		return ""
	}
	p, ok := parsePath(path)
	if !ok {
		return ""
	}
	seg := m.Segment(p.segment, p.occurrence)
	if seg == nil {
		return ""
	}
	field := seg.Field(p.field)
	if !p.hasRepetition && !p.hasComponent {
		return field.Value
	}
	rep := field.Repetition(p.repetition)
	if !p.hasComponent {
		return rep.Value
	}
	comp := rep.Component(p.component)
	if !p.hasSubcomponent {
		return comp.Value
	}
	return comp.Subcomponent(p.subcomponent)
}

type pathRef struct {
	segment                                      string
	occurrence, field, repetition                int
	component, subcomponent                      int
	hasRepetition, hasComponent, hasSubcomponent bool
}

// parsePath reads the path grammar documented on Value. It is deliberately
// strict: a path it cannot make sense of returns false rather than resolving to
// something adjacent, because a typo that silently reads the wrong field is far
// worse than one that reads nothing.
func parsePath(s string) (pathRef, bool) {
	p := pathRef{occurrence: 1, repetition: 1, component: 1, subcomponent: 1}
	s = strings.TrimSpace(s)

	cut := strings.IndexAny(s, "(-")
	if cut <= 0 {
		return p, false
	}
	p.segment = strings.ToUpper(s[:cut])
	s = s[cut:]

	if strings.HasPrefix(s, "(") {
		n, rest, ok := readIndex(s, "(", ")")
		if !ok {
			return p, false
		}
		p.occurrence, s = n, rest
	}
	if !strings.HasPrefix(s, "-") {
		return p, false
	}
	s = s[1:]

	digits := 0
	for digits < len(s) && s[digits] >= '0' && s[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return p, false
	}
	n, err := strconv.Atoi(s[:digits])
	if err != nil || n < 1 {
		return p, false
	}
	p.field, s = n, s[digits:]

	if strings.HasPrefix(s, "[") {
		n, rest, ok := readIndex(s, "[", "]")
		if !ok {
			return p, false
		}
		p.repetition, s, p.hasRepetition = n, rest, true
	}

	for _, into := range []struct {
		n   *int
		has *bool
	}{{&p.component, &p.hasComponent}, {&p.subcomponent, &p.hasSubcomponent}} {
		if s == "" {
			return p, true
		}
		if !strings.HasPrefix(s, ".") {
			return p, false
		}
		s = s[1:]
		digits := 0
		for digits < len(s) && s[digits] >= '0' && s[digits] <= '9' {
			digits++
		}
		if digits == 0 {
			return p, false
		}
		n, err := strconv.Atoi(s[:digits])
		if err != nil || n < 1 {
			return p, false
		}
		*into.n, *into.has, s = n, true, s[digits:]
	}
	return p, s == ""
}

// readIndex reads a bracketed 1-based index, returning what follows it.
func readIndex(s, open, closing string) (int, string, bool) {
	end := strings.Index(s, closing)
	if end < 0 {
		return 0, s, false
	}
	n, err := strconv.Atoi(s[len(open):end])
	if err != nil || n < 1 {
		return 0, s, false
	}
	return n, s[end+len(closing):], true
}

func itoa(n int) string { return strconv.Itoa(n) }

// quoteString renders an untrusted fragment for an issue message, so a segment
// id made of control characters cannot smuggle anything into a log line.
func quoteString(s string) string { return strconv.Quote(s) }
