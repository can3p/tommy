package mllp

import (
	"strings"
	"time"

	"github.com/can3p/tommy/plugins/hl7"
)

// ackCode is MSA-1.
type ackCode string

// The three codes this provider ever sends. HL7 also defines the "enhanced
// mode" CA/CE/CR triplet, but those exist to distinguish a transport-level
// commit acknowledgement from an application-level one in a two-phase
// exchange tommy never does - it always answers with a single, final
// acknowledgement, which is exactly what AA/AE/AR are for.
const (
	// ackAccept (AA) says the message was received and identified well
	// enough to correlate: there is a usable MSH.
	ackAccept ackCode = "AA"
	// ackError (AE) says a header was found but is unreliable enough that
	// parsing beyond it cannot be trusted - a mangled encoding-characters
	// declaration, an MSH that is not the first segment, or a separator
	// that collided with another one and was dropped. This matches the
	// conventional meaning of AE: something was parsed, but not cleanly.
	ackError ackCode = "AE"
	// ackReject (AR) says there was nothing to parse a header from at all -
	// no MSH, FHS or BHS segment anywhere in the frame. This matches the
	// conventional meaning of AR: the message is structurally invalid
	// (missing a required segment), not merely imperfect.
	ackReject ackCode = "AR"
)

// classify decides AA/AE/AR from what the parser recorded, per CLAUDE.md's
// scoping rule and the plan's explicit instruction: this is a mechanical
// reply derived from the request, never a decision about what the message
// means. HasHeader() and HasIssue(code) are exactly the accessors hl7's core
// exposes for it.
//
// Everything else the parser can flag - an unknown segment ID, a segment
// with no fields - does not affect whether the header itself can be trusted,
// so it does not change the acknowledgement; it is still visible on the
// captured event for a person to look at, which is the part of this project
// that is allowed to editorialize.
func classify(m *hl7.Message) ackCode {
	if !m.HasHeader() {
		return ackReject
	}
	if m.HasIssue(hl7.IssueNoEncodingCharacters) || m.HasIssue(hl7.IssueHeaderNotFirst) || m.HasIssue(hl7.IssueDuplicateSeparator) {
		return ackError
	}
	return ackAccept
}

// buildACK builds the wire bytes (unframed - see frame in framing.go) of the
// acknowledgement for a captured message.
//
// It is built with the same separators the inbound message declared for
// itself, taken from m.Separators - using the conventional |^~\& here
// instead would be the exact bug this plugin exists to expose. When the
// message carried no usable MSH at all (classify returned AR), there is
// nothing to take separators, a sender or a control id from: the
// conventional set is used because an integration engine expects an
// acknowledgement on every connection it opens, even one that never
// identified itself, and MSA-2 - the control id being acknowledged - is left
// empty, deliberately: there genuinely is not one, and a sender that could
// not even say what it sent is the party best placed to notice "" coming
// back.
//
// now is the ACK's own MSH-7; controlID is a fresh id for the ACK's own
// MSH-10, supplied by the caller (Deps.NewID) rather than generated here, so
// this function stays a pure translation with nothing to fake for a test.
func buildACK(m *hl7.Message, code ackCode, now time.Time, controlID string) []byte {
	hasHeader := m.HasHeader()
	seps := m.Separators
	if !hasHeader {
		seps = hl7.DefaultSeparators()
	}

	fs := seps.Field
	if fs == "" {
		fs = hl7.DefaultField
	}
	compSep := seps.Component
	if compSep == "" {
		compSep = hl7.DefaultComponent
	}
	enc := seps.EncodingCharacters()
	if len([]rune(enc)) < 4 {
		enc = hl7.DefaultComponent + hl7.DefaultRepetition + hl7.DefaultEscape + hl7.DefaultSubcomponent
	}

	var sendApp, sendFac, recvApp, recvFac, origControlID, processingID, version, triggerEvent string
	if hasHeader {
		// Sending and receiving application/facility swapped: tommy was the
		// receiver of the original message and is the sender of this one.
		sendApp = m.Header.ReceivingApplication
		sendFac = m.Header.ReceivingFacility
		recvApp = m.Header.SendingApplication
		recvFac = m.Header.SendingFacility
		origControlID = m.Header.ControlID
		processingID = m.Header.ProcessingID
		version = m.Header.Version
		triggerEvent = m.Header.TriggerEvent
	}

	esc := func(s string) string { return escapeValue(s, seps) }

	// MSH-9: "ACK", or "ACK^<trigger event>" when the original declared one.
	// Both forms are legal HL7; echoing the trigger event is more useful for
	// a person scanning captures side by side, and is exactly the kind of
	// information this tool exists to preserve rather than discard.
	msgType := "ACK"
	if triggerEvent != "" {
		msgType = "ACK" + compSep + esc(triggerEvent)
	}

	mshFields := []string{
		"MSH",
		enc,
		esc(sendApp),
		esc(sendFac),
		esc(recvApp),
		esc(recvFac),
		now.Format("20060102150405"), // MSH-7, HL7's own YYYYMMDDHHMMSS form
		"",                           // MSH-8 Security: nothing to echo
		msgType,
		controlID,
		processingID,
		version,
	}
	msh := mshFields[0] + fs + strings.Join(mshFields[1:], fs)
	msa := "MSA" + fs + string(code) + fs + esc(origControlID)

	return []byte(msh + "\r" + msa + "\r")
}

// escapeValue re-encodes a value already decoded by hl7.Parse (Message.Value,
// Header fields) back into wire form using seps' own delimiters, so a value
// that happens to contain one of them - most plausibly the original
// message's own control id, echoed back verbatim in MSA-2 - cannot corrupt
// the ACK's structure. It mirrors parse.go's expandEscape in reverse: the
// five delimiter escapes the standard defines, nothing invented.
func escapeValue(s string, seps hl7.Separators) string {
	if s == "" {
		return s
	}
	esc := seps.Escape
	if esc == "" {
		esc = hl7.DefaultEscape
	}

	// The escape character itself must be replaced first, so that the
	// escape sequences this loop inserts for the other separators are not
	// themselves mistaken for literal separators on a second pass -
	// strings.Replacer applies all pairs in a single left-to-right scan, so
	// there is no second pass, but keeping the escape character first in
	// the pair list keeps the intent explicit.
	pairs := []string{esc, esc + "E" + esc}
	add := func(sep, code string) {
		if sep != "" && sep != esc {
			pairs = append(pairs, sep, esc+code+esc)
		}
	}
	add(seps.Field, "F")
	add(seps.Component, "S")
	add(seps.Repetition, "R")
	add(seps.Subcomponent, "T")

	return strings.NewReplacer(pairs...).Replace(s)
}
