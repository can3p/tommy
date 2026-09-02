package hl7

import (
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"
)

// ErrEmpty is the only way parsing fails outright: there was nothing in the
// bytes that could be a segment.
//
// Everything else - a fragment with no MSH, a truncated line, an unknown
// segment id, a header that declared nonsense separators - parses to whatever
// can be recovered, with an Issue recorded against it. Refusing to show a
// malformed message would defeat the point of capturing it: the reason to look
// at a captured message is usually that something about it is wrong.
var ErrEmpty = errors.New("hl7: message contains no segments")

// MLLP framing bytes. The MLLP provider strips these itself, but a message
// pasted or piped in by hand often still carries them, and a stray 0x0B is not
// a reason to mangle the first segment id.
const (
	mllpStart = '\x0b'
	mllpEnd   = '\x1c'
)

// headerSegments are the segment ids that carry the encoding characters in
// their second field. MSH heads a message; FHS and BHS head a file or a batch
// of them and follow exactly the same convention.
var headerSegments = map[string]bool{"MSH": true, "FHS": true, "BHS": true}

func isHeaderSegment(id string) bool { return headerSegments[strings.ToUpper(id)] }

// Parse turns the bytes of an HL7 v2 message into the canonical model.
//
// The separators come from the message itself: MSH-1 is the character
// immediately after the segment id and MSH-2 is the component, repetition,
// escape and subcomponent separators in that order. Nothing here assumes |^~\&,
// because plenty of real senders do not use it.
//
// Segments are split on \r, \n or \r\n. The standard says \r, but a message
// that has been through a text editor, a shell heredoc or an HTTP client on the
// way here will not have one, and being strict about it would only mean showing
// the whole message as a single unusable segment.
//
// The input bytes are never modified and never need to be kept in sync with the
// model: put them in Raw.Body verbatim.
func Parse(raw []byte) (*Message, error) { return ParseString(string(raw)) }

// ParseString is Parse for a string.
func ParseString(text string) (*Message, error) {
	lines := splitSegments(text)
	if len(lines) == 0 {
		return nil, ErrEmpty
	}

	m := &Message{}
	m.Separators, m.Issues = readSeparators(lines)

	occurrences := make(map[string]int, len(lines))
	for i, line := range lines {
		id, seg, issues := parseSegment(line, m.Separators)
		occurrences[id]++
		seg.Index = i + 1
		seg.Occurrence = occurrences[id]
		m.Segments = append(m.Segments, seg)
		for _, issue := range issues {
			issue.Detail = "segment " + itoa(i+1) + ": " + issue.Detail
			m.Issues = append(m.Issues, issue)
		}
	}

	m.deriveHeader()
	return m, nil
}

// splitSegments breaks the message into segment lines, tolerating every line
// ending in the wild and the MLLP framing bytes.
func splitSegments(text string) []string {
	text = strings.TrimLeft(text, string(mllpStart))
	text = strings.TrimRight(text, string(mllpEnd)+"\r\n")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	var out []string
	for _, line := range strings.Split(text, "\n") {
		// A segment could legitimately end in a meaningful space, so only a
		// line that is *entirely* blank is dropped.
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, strings.Trim(line, string(mllpStart)+string(mllpEnd)))
	}
	return out
}

// readSeparators recovers the delimiters the message declared, falling back to
// the conventional set one character at a time.
//
// A separator that repeats one already claimed is dropped rather than used: two
// meanings for one character cannot both be right, and splitting on it anyway
// would shred every value in the message rather than just the header.
func readSeparators(lines []string) (Separators, []Issue) {
	var issues []Issue

	header, index := "", -1
	for i, line := range lines {
		if len(line) >= 3 && isHeaderSegment(line[:3]) {
			header, index = line, i
			break
		}
	}
	switch {
	case index < 0:
		return DefaultSeparators(), []Issue{{
			Code:   IssueNoHeader,
			Detail: "no MSH, FHS or BHS segment, so the conventional |^~\\& separators were assumed",
		}}
	case index > 0:
		issues = append(issues, Issue{
			Code:   IssueHeaderNotFirst,
			Detail: "the " + header[:3] + " segment is at position " + itoa(index+1) + ", not first",
		})
	}

	rest := header[3:]
	if rest == "" {
		return DefaultSeparators(), append(issues, Issue{
			Code:   IssueNoEncodingCharacters,
			Detail: header[:3] + " declared no field separator, so the conventional |^~\\& separators were assumed",
		})
	}

	field, size := utf8.DecodeRuneInString(rest)
	seps := Separators{Field: string(field)}
	rest = rest[size:]

	// MSH-2 runs to the next field separator; it is never split further, since
	// every character in it is a separator rather than a value.
	encoding := rest
	if end := strings.Index(rest, seps.Field); end >= 0 {
		encoding = rest[:end]
	}
	enc := []rune(encoding)
	if len(enc) < 4 {
		issues = append(issues, Issue{
			Code: IssueNoEncodingCharacters,
			Detail: header[:3] + "-2 carries " + itoa(len(enc)) + " of the 4 encoding characters, " +
				"so the missing ones were defaulted",
		})
	}
	at := func(i int, def string) string {
		if i < len(enc) {
			return string(enc[i])
		}
		return def
	}
	seps.Component = at(0, DefaultComponent)
	seps.Repetition = at(1, DefaultRepetition)
	seps.Escape = at(2, DefaultEscape)
	seps.Subcomponent = at(3, DefaultSubcomponent)

	// Drop duplicates in declaration order, so the field separator always wins.
	seen := map[string]string{seps.Field: "field"}
	for _, s := range []struct {
		name string
		into *string
	}{
		{"component", &seps.Component},
		{"repetition", &seps.Repetition},
		{"escape", &seps.Escape},
		{"subcomponent", &seps.Subcomponent},
	} {
		if owner, dup := seen[*s.into]; dup {
			issues = append(issues, Issue{
				Code: IssueDuplicateSeparator,
				Detail: "the " + s.name + " separator " + quoteString(*s.into) + " is already the " + owner +
					" separator, so nothing is split on it",
			})
			*s.into = ""
			continue
		}
		seen[*s.into] = s.name
	}
	return seps, issues
}

// parseSegment splits one line into fields. It returns the segment id
// separately because the caller numbers occurrences before the segment is
// complete.
func parseSegment(line string, seps Separators) (string, Segment, []Issue) {
	var issues []Issue

	parts := []string{line}
	if seps.Field != "" {
		parts = strings.Split(line, seps.Field)
	}
	id := parts[0]
	seg := Segment{ID: id}

	if utf8.RuneCountInString(id) != 3 {
		issues = append(issues, Issue{
			Code:   IssueSegmentID,
			Detail: "id " + quoteString(id) + " is not the three characters HL7 requires",
		})
	}
	if len(parts) == 1 {
		return id, seg, append(issues, Issue{
			Code:   IssueNoFields,
			Detail: "id " + quoteString(id) + " with no fields after it",
		})
	}

	if isHeaderSegment(id) {
		// MSH-1 *is* the field separator, so it never appears as a value in
		// the split, and MSH-2 is the encoding characters, which must not be
		// split or unescaped by the very separators they declare. Both are
		// taken verbatim; everything from MSH-3 on is an ordinary field.
		seg.Fields = append(seg.Fields, verbatimField(1, seps.Field))
		seg.Fields = append(seg.Fields, verbatimField(2, parts[1]))
		for i, raw := range parts[2:] {
			seg.Fields = append(seg.Fields, parseField(i+3, raw, seps))
		}
		return id, seg, issues
	}

	for i, raw := range parts[1:] {
		seg.Fields = append(seg.Fields, parseField(i+1, raw, seps))
	}
	return id, seg, issues
}

// verbatimField is a field kept exactly as it arrived: no splitting, no escape
// decoding. Only MSH-1 and MSH-2 need it.
func verbatimField(pos int, raw string) Field {
	f := Field{Position: pos, Value: raw}
	if raw == "" {
		return f
	}
	f.Repetitions = []Repetition{{
		Value:      raw,
		Components: []Component{{Value: raw, Subcomponents: []string{raw}}},
	}}
	return f
}

func parseField(pos int, raw string, seps Separators) Field {
	f := Field{Position: pos}
	if raw == "" {
		return f
	}
	for _, rep := range split(raw, seps.Repetition) {
		f.Repetitions = append(f.Repetitions, parseRepetition(rep, seps))
	}
	f.Value = joinValues(f.Repetitions, func(r Repetition) string { return r.Value }, seps.Repetition, DefaultRepetition)
	return f
}

func parseRepetition(raw string, seps Separators) Repetition {
	r := Repetition{}
	if raw == "" {
		return r
	}
	for _, comp := range split(raw, seps.Component) {
		r.Components = append(r.Components, parseComponent(comp, seps))
	}
	r.Value = joinValues(r.Components, func(c Component) string { return c.Value }, seps.Component, DefaultComponent)
	return r
}

func parseComponent(raw string, seps Separators) Component {
	c := Component{}
	if raw == "" {
		return c
	}
	for _, sub := range split(raw, seps.Subcomponent) {
		c.Subcomponents = append(c.Subcomponents, decodeEscapes(sub, seps))
	}
	sep := seps.Subcomponent
	if sep == "" {
		sep = DefaultSubcomponent
	}
	c.Value = strings.Join(c.Subcomponents, sep)
	return c
}

// split is strings.Split with "no separator declared" meaning "do not split",
// which is what a duplicated or missing encoding character leaves behind.
func split(s, sep string) []string {
	if sep == "" {
		return []string{s}
	}
	return strings.Split(s, sep)
}

func joinValues[T any](items []T, value func(T) string, sep, def string) string {
	if sep == "" {
		sep = def
	}
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = value(item)
	}
	return strings.Join(parts, sep)
}

// decodeEscapes expands HL7's escape sequences into the characters they stand
// for, so a value reads as it was meant to rather than as \F\ and \S\.
//
// The five delimiter escapes (\F\ \S\ \T\ \R\ \E\) are the ones the standard
// requires and the ones that would otherwise corrupt a value. \X..\ hex escapes
// are decoded too, since they are how anything outside the character set gets
// sent, and \.br\ becomes a newline because free-text NTE and OBX values lean on
// it heavily.
//
// Every other sequence - the rest of the formatting commands, locally defined
// \Z..\ escapes - is left exactly as it arrived. Guessing at what a sender's
// private escape means would be inventing content, and an escape nobody decoded
// is still readable; one that was decoded wrongly is not.
//
// An unterminated escape is literal text. Swallowing the rest of the value
// because somebody sent a lone backslash would lose real content.
func decodeEscapes(s string, seps Separators) string {
	esc := seps.Escape
	if esc == "" || !strings.Contains(s, esc) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	for {
		start := strings.Index(s, esc)
		if start < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:start])
		rest := s[start+len(esc):]
		end := strings.Index(rest, esc)
		if end < 0 {
			// No closing delimiter: the escape character is just a character.
			b.WriteString(s[start:])
			return b.String()
		}
		seq := rest[:end]
		if expanded, ok := expandEscape(seq, seps); ok {
			b.WriteString(expanded)
		} else {
			b.WriteString(esc + seq + esc)
		}
		s = rest[end+len(esc):]
	}
}

// expandEscape resolves one escape sequence, reporting false for anything it
// deliberately leaves alone.
func expandEscape(seq string, seps Separators) (string, bool) {
	switch seq {
	case "F":
		return seps.Field, true
	case "S":
		return seps.Component, true
	case "T":
		return seps.Subcomponent, true
	case "R":
		return seps.Repetition, true
	case "E":
		return seps.Escape, true
	case ".br":
		return "\n", true
	}
	if len(seq) > 1 && (seq[0] == 'X' || seq[0] == 'x') {
		if decoded, err := hex.DecodeString(seq[1:]); err == nil {
			return string(decoded), true
		}
	}
	return "", false
}

// deriveHeader lifts the fields every read surface names a message by out of
// MSH, once. Doing it at parse time is what stops the list, the API and the
// event summary from each deriving it slightly differently.
func (m *Message) deriveHeader() {
	var seg *Segment
	for i := range m.Segments {
		if isHeaderSegment(m.Segments[i].ID) {
			seg = &m.Segments[i]
			break
		}
	}
	if seg == nil {
		return
	}
	first := func(pos, comp int) string {
		return seg.Field(pos).Repetition(1).Component(comp).Value
	}
	m.Header = Header{
		SendingApplication:   first(3, 1),
		SendingFacility:      first(4, 1),
		ReceivingApplication: first(5, 1),
		ReceivingFacility:    first(6, 1),
		Timestamp:            first(7, 1),
		Code:                 first(9, 1),
		TriggerEvent:         first(9, 2),
		Structure:            first(9, 3),
		ControlID:            first(10, 1),
		ProcessingID:         first(11, 1),
		Version:              first(12, 1),
	}
}
