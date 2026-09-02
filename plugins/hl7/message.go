// Package hl7 is tommy's HL7 v2 content type: the canonical Message every HL7
// provider converts into, the read-back API under /api/v1/hl7/ and the segment
// tree tab under /ui/hl7/.
//
// Providers (MLLP, and whatever comes later) live in plugins/hl7/providers/...
// and never import each other; all they share is the Message in this file and
// the parser in parse.go.
//
// Three design points are load bearing:
//
//   - The separators are read from the message, never assumed. MSH-1 is the
//     field separator and MSH-2 carries the component, repetition, escape and
//     subcomponent separators. A parser that hardcodes |^~\& is wrong on real
//     traffic, and that is the single most common HL7 parsing bug.
//   - The hierarchy is kept whole: segment, field, repetition, component,
//     subcomponent. Repetitions are never flattened into one joined string,
//     because seeing that PID-3 repeats is most of the reason to look at a
//     captured message at all.
//   - Everything in here is untrusted. HL7 carries patient names and free-text
//     notes written by whatever system is under test; every read surface
//     interpolates it as a plain string through html/template and none of them
//     renders it as HTML.
package hl7

import (
	"strconv"
	"strings"
)

// Name is the plugin name and the URL segment it is mounted under.
const Name = "hl7"

// EventType is the event.Type every captured message carries. A provider that
// grows another resource later adds a new type rather than overloading this
// one, and every read surface switches on the type instead of assuming it.
const EventType = "hl7.message"

// Transport is the Raw.Transport an HL7 provider records. MLLP is a framing
// over a plain TCP stream, so there is nothing more specific to say.
const Transport = "tcp"

// Separators are the delimiters a message declared for itself in MSH-1 and
// MSH-2. They are strings rather than runes so the JSON an API hands back is
// readable, and so "this message never declared one" is expressible as "".
//
// A separator that a message did not declare, or that duplicates one already
// claimed, is left empty and the parser simply does not split on it. That is
// what keeps a malformed header from silently shredding every value.
type Separators struct {
	Field        string `json:"field"`
	Component    string `json:"component,omitempty"`
	Repetition   string `json:"repetition,omitempty"`
	Escape       string `json:"escape,omitempty"`
	Subcomponent string `json:"subcomponent,omitempty"`
}

// The delimiters HL7 v2 conventionally uses, and what the parser falls back to
// for a fragment that carries no MSH to declare its own.
const (
	DefaultField        = "|"
	DefaultComponent    = "^"
	DefaultRepetition   = "~"
	DefaultEscape       = `\`
	DefaultSubcomponent = "&"
)

// DefaultSeparators is the conventional |^~\& set.
func DefaultSeparators() Separators {
	return Separators{
		Field:        DefaultField,
		Component:    DefaultComponent,
		Repetition:   DefaultRepetition,
		Escape:       DefaultEscape,
		Subcomponent: DefaultSubcomponent,
	}
}

// EncodingCharacters is MSH-2 as the message spelled it: the component,
// repetition, escape and subcomponent separators concatenated.
func (s Separators) EncodingCharacters() string {
	return s.Component + s.Repetition + s.Escape + s.Subcomponent
}

// Standard reports whether the message used the conventional |^~\& set, which
// is worth saying out loud in the UI precisely because the unconventional case
// is the one that breaks other people's parsers.
func (s Separators) Standard() bool { return s == DefaultSeparators() }

// Component is one component of a field repetition - PID-5.1, the family name
// of a patient name.
//
// Value is the component's text with escape sequences already decoded; when it
// has more than one subcomponent, Value is those joined back together with the
// message's own subcomponent separator. Subcomponents always carries at least
// one entry for a non-empty component, so a view never has to special-case the
// flat shape.
type Component struct {
	Value         string   `json:"value"`
	Subcomponents []string `json:"subcomponents,omitempty"`
}

// HasSubcomponents reports whether the component splits any further.
func (c Component) HasSubcomponents() bool { return len(c.Subcomponents) > 1 }

// Subcomponent returns the 1-based subcomponent, or "" when there is none.
func (c Component) Subcomponent(n int) string {
	if n < 1 || n > len(c.Subcomponents) {
		return ""
	}
	return c.Subcomponents[n-1]
}

// Repetition is one occurrence of a repeating field. PID-3 may carry an MRN and
// an account number; each is a Repetition with components of its own.
type Repetition struct {
	Value      string      `json:"value"`
	Components []Component `json:"components,omitempty"`
}

// HasComponents reports whether the repetition splits into more than one
// component, which is what decides whether a view shows PID-5 or PID-5.1.
func (r Repetition) HasComponents() bool { return len(r.Components) > 1 }

// Component returns the 1-based component, or the zero Component.
func (r Repetition) Component(n int) Component {
	if n < 1 || n > len(r.Components) {
		return Component{}
	}
	return r.Components[n-1]
}

// Field is one field of a segment, at its 1-based position: PID-5 is
// Position 5. An empty field carries no repetitions at all.
type Field struct {
	Position    int          `json:"position"`
	Value       string       `json:"value"`
	Repetitions []Repetition `json:"repetitions,omitempty"`
}

// Empty reports whether the field carried nothing.
func (f Field) Empty() bool { return len(f.Repetitions) == 0 }

// Repeats reports whether the field occurred more than once.
func (f Field) Repeats() bool { return len(f.Repetitions) > 1 }

// Repetition returns the 1-based repetition, or the zero Repetition.
func (f Field) Repetition(n int) Repetition {
	if n < 1 || n > len(f.Repetitions) {
		return Repetition{}
	}
	return f.Repetitions[n-1]
}

// Segment is one line of the message: a three-character id and its fields.
//
// Index is the segment's 1-based position in the message and Occurrence is its
// 1-based position among the segments sharing its id, so the third OBX of an
// ORU can be named without counting.
type Segment struct {
	ID         string  `json:"id"`
	Index      int     `json:"index"`
	Occurrence int     `json:"occurrence"`
	Fields     []Field `json:"fields,omitempty"`
}

// Field returns the field at the 1-based position, or the zero Field. MSH-1 is
// synthesized by the parser, so this answers for it like any other position.
func (s Segment) Field(pos int) Field {
	if pos < 1 || pos > len(s.Fields) {
		return Field{}
	}
	return s.Fields[pos-1]
}

// Label names the segment for a heading: "OBX" or, when it repeats, "OBX #2".
func (s Segment) Label() string {
	if s.Occurrence > 1 {
		return s.ID + " #" + strconv.Itoa(s.Occurrence)
	}
	return s.ID
}

// Issue is something the parser found wrong but recovered from. Parsing an HL7
// message practically never fails outright - a truncated or headerless message
// is still worth showing, since seeing what actually arrived is the whole point
// of capturing it - so problems are recorded here instead of thrown away.
//
// Code is stable and machine-readable; a provider deciding between an AA and an
// AE acknowledgement switches on it rather than on the prose.
type Issue struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// The issue codes the parser emits.
const (
	// IssueNoHeader: the message carried no MSH (or FHS/BHS) segment, so the
	// conventional separators were assumed.
	IssueNoHeader = "no-header"
	// IssueHeaderNotFirst: an MSH segment exists but is not the first one.
	IssueHeaderNotFirst = "header-not-first"
	// IssueNoEncodingCharacters: MSH-2 was missing or short, so the missing
	// separators were defaulted.
	IssueNoEncodingCharacters = "no-encoding-characters"
	// IssueDuplicateSeparator: MSH-2 reused a character already claimed by
	// another separator, so the duplicate is not split on.
	IssueDuplicateSeparator = "duplicate-separator"
	// IssueSegmentID: a segment id is not the three characters HL7 requires.
	IssueSegmentID = "segment-id"
	// IssueNoFields: a segment carried an id and nothing else.
	IssueNoFields = "no-fields"
)

// Header is what MSH says about the message: who sent it to whom, what it is,
// and which conversation it belongs to. It is derived from the segments at parse
// time so that every read surface - the list, the API, the event summary - names
// a message the same way.
type Header struct {
	SendingApplication   string `json:"sending_application,omitempty"`
	SendingFacility      string `json:"sending_facility,omitempty"`
	ReceivingApplication string `json:"receiving_application,omitempty"`
	ReceivingFacility    string `json:"receiving_facility,omitempty"`
	// Timestamp is MSH-7 exactly as sent, in HL7's own YYYYMMDDHHMMSS form.
	// It is not parsed into a time.Time: it may carry a precision and an
	// offset that a Go time would quietly normalise away, and what was sent is
	// what this tool exists to show.
	Timestamp string `json:"timestamp,omitempty"`
	// Code, TriggerEvent and Structure are MSH-9.1, 9.2 and 9.3: "ADT", "A01",
	// "ADT_A01".
	Code         string `json:"code,omitempty"`
	TriggerEvent string `json:"trigger_event,omitempty"`
	Structure    string `json:"structure,omitempty"`
	ControlID    string `json:"control_id,omitempty"`
	ProcessingID string `json:"processing_id,omitempty"`
	Version      string `json:"version,omitempty"`
}

// MessageType is the label a person scans a list for: "ADT^A01", or just the
// code when no trigger event was given.
//
// The caret here is the canonical HL7 component separator, not the one this
// particular message declared. It is a label rather than a slice of the wire
// format, and a label that changes shape with the sender is a label nobody can
// search for.
func (h Header) MessageType() string {
	if h.Code == "" && h.TriggerEvent == "" {
		return ""
	}
	if h.TriggerEvent == "" {
		return h.Code
	}
	return h.Code + DefaultComponent + h.TriggerEvent
}

// Sender is "application / facility", or whichever of the two was given.
func (h Header) Sender() string { return joinParty(h.SendingApplication, h.SendingFacility) }

// Receiver is "application / facility", or whichever of the two was given.
func (h Header) Receiver() string { return joinParty(h.ReceivingApplication, h.ReceivingFacility) }

func joinParty(app, facility string) string {
	switch {
	case app != "" && facility != "":
		return app + " / " + facility
	case app != "":
		return app
	default:
		return facility
	}
}

// Message is the HL7 plugin's canonical model: what every provider converts its
// wire format into, and what lands in event.Payload.
//
// Provider-specific metadata - the peer address, the MLLP framing it arrived
// in, how long the connection had been open - belongs in Event.Meta, not here.
// This struct only carries what the message itself said.
type Message struct {
	// Separators are the delimiters this message declared for itself.
	Separators Separators `json:"separators"`
	// Header is MSH, pulled out for the surfaces that name a message.
	Header Header `json:"header"`
	// Segments are every segment in the order it arrived.
	Segments []Segment `json:"segments,omitempty"`
	// Issues is everything the parser recovered from. Empty for a well-formed
	// message.
	Issues []Issue `json:"issues,omitempty"`
}

// HasHeader reports whether the message actually carried an MSH segment. A
// provider generating an acknowledgement needs this: there is nothing to echo
// back to a fragment that never identified itself.
func (m *Message) HasHeader() bool {
	if m == nil {
		return false
	}
	for _, s := range m.Segments {
		if isHeaderSegment(s.ID) {
			return true
		}
	}
	return false
}

// HasIssue reports whether the parser recorded an issue with the given code.
func (m *Message) HasIssue(code string) bool {
	if m == nil {
		return false
	}
	for _, i := range m.Issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

// SegmentIDs lists every segment id in order, which is the outline of the
// message and the cheapest useful preview of one.
func (m *Message) SegmentIDs() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.Segments))
	for _, s := range m.Segments {
		out = append(out, s.ID)
	}
	return out
}

// Outline collapses the segment ids into a scannable line, counting runs of the
// same segment: "MSH PID PV1 OBX x3".
func (m *Message) Outline() string {
	ids := m.SegmentIDs()
	var parts []string
	for i := 0; i < len(ids); {
		j := i
		for j < len(ids) && ids[j] == ids[i] {
			j++
		}
		if n := j - i; n > 1 {
			parts = append(parts, ids[i]+" x"+strconv.Itoa(n))
		} else {
			parts = append(parts, ids[i])
		}
		i = j
	}
	return strings.Join(parts, " ")
}

// Segment returns the nth (1-based) occurrence of a segment id, or nil.
func (m *Message) Segment(id string, occurrence int) *Segment {
	if m == nil || occurrence < 1 {
		return nil
	}
	id = strings.ToUpper(id)
	for i := range m.Segments {
		if m.Segments[i].ID == id && m.Segments[i].Occurrence == occurrence {
			return &m.Segments[i]
		}
	}
	return nil
}

// SegmentsByID returns every segment with the given id, in order.
func (m *Message) SegmentsByID(id string) []*Segment {
	if m == nil {
		return nil
	}
	id = strings.ToUpper(id)
	var out []*Segment
	for i := range m.Segments {
		if m.Segments[i].ID == id {
			out = append(out, &m.Segments[i])
		}
	}
	return out
}

// PatientName is PID-5 of the first PID segment, rendered with the canonical
// component separator: "DOE^JOHN^A". Empty when the message has no PID, which
// is most of them.
//
// It exists because a list of captured messages that says only "ADT^A01" is
// nearly useless for finding the one you just sent, and the patient name is
// what people actually recognize.
func (m *Message) PatientName() string {
	seg := m.Segment("PID", 1)
	if seg == nil {
		return ""
	}
	rep := seg.Field(5).Repetition(1)
	if len(rep.Components) <= 1 {
		return rep.Value
	}
	parts := make([]string, 0, len(rep.Components))
	for _, c := range rep.Components {
		parts = append(parts, c.Value)
	}
	// Trailing components are almost always empty padding; drop them so a name
	// does not read as "DOE^JOHN^^^^^".
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return strings.Join(parts, DefaultComponent)
}

// Title names the message for a list or an event summary.
func (m *Message) Title() string {
	if m == nil {
		return "HL7 message"
	}
	t := m.Header.MessageType()
	if t == "" {
		t = "HL7 message"
	}
	if m.Header.ControlID != "" {
		return t + " · " + m.Header.ControlID
	}
	return t
}

// Preview is the one-line description a list row and the store's search index
// see: the patient, when there is one, and the segment outline.
func (m *Message) Preview() string {
	if m == nil {
		return ""
	}
	outline := m.Outline()
	if name := m.PatientName(); name != "" {
		if outline == "" {
			return name
		}
		return name + " · " + outline
	}
	return outline
}
