package hl7_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/can3p/tommy/plugins/hl7"
)

// fixture reads a golden message. The files carry \r line endings, the way real
// HL7 does, so a test that passes here is not quietly relying on a text editor
// having rewritten them.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func parseFixture(t *testing.T, name string) *hl7.Message {
	t.Helper()
	m, err := hl7.Parse(fixture(t, name))
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return m
}

// TestParseFixtures asserts the canonical model each golden message produces:
// the separators it declared, the header derived from MSH, the segment outline,
// and the values at the paths that matter.
func TestParseFixtures(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		separators hl7.Separators
		outline    string
		header     hl7.Header
		values     map[string]string
		issues     []string
	}{
		{
			name:       "ADT^A01 with the conventional separators",
			file:       "adt_a01.hl7",
			separators: hl7.DefaultSeparators(),
			outline:    "MSH EVN PID PV1 NTE",
			header: hl7.Header{
				SendingApplication:   "EPICADT",
				SendingFacility:      "EPIC_FAC",
				ReceivingApplication: "SMSADT",
				ReceivingFacility:    "SMS_FAC",
				Timestamp:            "202401011230",
				Code:                 "ADT",
				TriggerEvent:         "A01",
				Structure:            "ADT_A01",
				ControlID:            "MSG00001",
				ProcessingID:         "P",
				Version:              "2.5",
			},
			values: map[string]string{
				"MSH-1":      "|",
				"MSH-2":      `^~\&`,
				"MSH-9.1":    "ADT",
				"MSH-9.2":    "A01",
				"MSH-10":     "MSG00001",
				"EVN-1":      "A01",
				"PID-3[1].1": "MRN12345",
				"PID-3[1].5": "MR",
				"PID-3[2].1": "999887777",
				"PID-3[2].5": "SS",
				"PID-5.1":    "DOE",
				"PID-5.2":    "JOHN",
				"PID-8":      "M",
				// \S\ is an escaped component separator, so this reads as a
				// value rather than splitting the address into more components.
				"PID-11.2":  "APT ^4^",
				"PV1-3.4.2": "HOSPITAL MAIN",
				"PV1-7.2":   "SMITH",
				"NTE-3":     "Fever & chills reported\nReview in 2 weeks",
			},
		},
		{
			name:       "ORU^R01 with repeating OBX segments",
			file:       "oru_r01.hl7",
			separators: hl7.DefaultSeparators(),
			outline:    "MSH PID OBR OBX x3 NTE",
			header: hl7.Header{
				SendingApplication:   "LIS",
				SendingFacility:      "LAB_FAC",
				ReceivingApplication: "EHR",
				ReceivingFacility:    "HOSPITAL",
				Timestamp:            "20240102081500",
				Code:                 "ORU",
				TriggerEvent:         "R01",
				Structure:            "ORU_R01",
				ControlID:            "MSG00002",
				ProcessingID:         "P",
				Version:              "2.5.1",
			},
			values: map[string]string{
				"OBR-4.2":    "COMPLETE BLOOD COUNT",
				"OBX(1)-3.1": "WBC",
				"OBX(1)-5":   "7.2",
				"OBX(2)-3.1": "HGB",
				"OBX(2)-5":   "13.5",
				"OBX(3)-5":   "Sample slightly hemolysed",
				"OBX(3)-8":   "A",
			},
		},
		{
			name: "separators declared by the message rather than assumed",
			file: "custom_separators.hl7",
			separators: hl7.Separators{
				Field: "!", Component: "@", Repetition: "#", Escape: "$", Subcomponent: "%",
			},
			outline: "MSH PID PV1 NTE",
			header: hl7.Header{
				SendingApplication:   "SENDER",
				SendingFacility:      "SFAC",
				ReceivingApplication: "RECEIVER",
				ReceivingFacility:    "RFAC",
				Timestamp:            "20240103120000",
				Code:                 "ADT",
				TriggerEvent:         "A04",
				Structure:            "ADT_A01",
				ControlID:            "MSG00003",
				ProcessingID:         "P",
				Version:              "2.3",
			},
			values: map[string]string{
				"MSH-1":      "!",
				"MSH-2":      "@#$%",
				"PID-3[1].1": "MRN777",
				"PID-3[2].1": "ALT888",
				"PID-5.1":    "BROWN",
				"PID-5.7":    "L",
				"PV1-3.2.2":  "WEST WING",
				// \F\ and \S\ expand to the separators *this* message
				// declared, not to | and ^.
				"NTE-3": "Sent ! separated with a literal @ inside",
			},
		},
		{
			name:       "a fragment with no MSH parses with the conventional separators",
			file:       "no_msh.hl7",
			separators: hl7.DefaultSeparators(),
			outline:    "PID PV1",
			header:     hl7.Header{},
			values: map[string]string{
				"PID-5.1": "FRAGMENT",
				"PV1-2":   "O",
			},
			issues: []string{hl7.IssueNoHeader},
		},
		{
			name:       "a truncated header keeps what it managed to send",
			file:       "truncated.hl7",
			separators: hl7.DefaultSeparators(),
			outline:    "MSH",
			header:     hl7.Header{SendingApplication: "SENDER"},
			values:     map[string]string{"MSH-3": "SENDER", "MSH-9": ""},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := parseFixture(t, tc.file)

			if m.Separators != tc.separators {
				t.Errorf("separators = %+v, want %+v", m.Separators, tc.separators)
			}
			if got := m.Outline(); got != tc.outline {
				t.Errorf("Outline() = %q, want %q", got, tc.outline)
			}
			if m.Header != tc.header {
				t.Errorf("Header = %+v\nwant           %+v", m.Header, tc.header)
			}
			for path, want := range tc.values {
				if got := m.Value(path); got != want {
					t.Errorf("Value(%q) = %q, want %q", path, got, want)
				}
			}
			var codes []string
			for _, i := range m.Issues {
				codes = append(codes, i.Code)
			}
			if strings.Join(codes, ",") != strings.Join(tc.issues, ",") {
				t.Errorf("issues = %v, want %v", codes, tc.issues)
			}
		})
	}
}

// The whole point of modeling repetitions instead of joining them: PID-3 must
// arrive as two identifiers, not as one string with a tilde in it.
func TestRepetitionsAreNotFlattened(t *testing.T) {
	m := parseFixture(t, "adt_a01.hl7")
	pid := m.Segment("PID", 1)
	if pid == nil {
		t.Fatal("no PID segment")
	}
	f := pid.Field(3)
	if !f.Repeats() {
		t.Fatalf("PID-3 does not report repeating: %+v", f)
	}
	if len(f.Repetitions) != 2 {
		t.Fatalf("PID-3 has %d repetitions, want 2", len(f.Repetitions))
	}
	if got := f.Repetition(1).Component(1).Value; got != "MRN12345" {
		t.Errorf("PID-3[1].1 = %q, want MRN12345", got)
	}
	if got := f.Repetition(2).Component(1).Value; got != "999887777" {
		t.Errorf("PID-3[2].1 = %q, want 999887777", got)
	}
	// The flattened value is still available, and still says it repeated.
	if want := "MRN12345^^^HOSP^MR~999887777^^^SSA^SS"; f.Value != want {
		t.Errorf("PID-3 value = %q, want %q", f.Value, want)
	}
}

func TestSubcomponents(t *testing.T) {
	m := parseFixture(t, "adt_a01.hl7")
	comp := m.Segment("PV1", 1).Field(3).Repetition(1).Component(4)
	if !comp.HasSubcomponents() {
		t.Fatalf("PV1-3.4 does not report subcomponents: %+v", comp)
	}
	want := []string{"HOSP", "HOSPITAL MAIN", "L"}
	if len(comp.Subcomponents) != len(want) {
		t.Fatalf("PV1-3.4 has %d subcomponents, want %d", len(comp.Subcomponents), len(want))
	}
	for i, w := range want {
		if comp.Subcomponents[i] != w {
			t.Errorf("PV1-3.4.%d = %q, want %q", i+1, comp.Subcomponents[i], w)
		}
	}
	if comp.Value != "HOSP&HOSPITAL MAIN&L" {
		t.Errorf("PV1-3.4 value = %q", comp.Value)
	}
}

// Real HL7 ends a segment with \r. Everything that has been through a text
// editor, a heredoc or an HTTP client will not, so all three must work and must
// produce the same model.
func TestLineEndings(t *testing.T) {
	base := `MSH|^~\&|A|B|C|D|20240101||ADT^A01|ID1|P|2.5` + "\x01" + `PID|1||MRN||DOE^JOHN`

	want := parseFixture(t, "adt_a01_lf.hl7").Value("PID-5.1")
	if want != "DOE" {
		t.Fatalf("the LF fixture did not parse: PID-5.1 = %q", want)
	}

	for _, term := range []string{"\r", "\n", "\r\n"} {
		text := strings.ReplaceAll(base, "\x01", term)
		m, err := hl7.ParseString(text)
		if err != nil {
			t.Fatalf("terminator %q: %v", term, err)
		}
		if len(m.Segments) != 2 {
			t.Errorf("terminator %q: %d segments, want 2", term, len(m.Segments))
		}
		if got := m.Value("PID-5.2"); got != "JOHN" {
			t.Errorf("terminator %q: PID-5.2 = %q, want JOHN", term, got)
		}
	}
}

func TestSeparatorRecovery(t *testing.T) {
	tests := []struct {
		name  string
		msg   string
		seps  hl7.Separators
		issue string
		check func(t *testing.T, m *hl7.Message)
	}{
		{
			name:  "MSH-2 shorter than four characters defaults the rest",
			msg:   `MSH|^~|A|B|C|D|20240101||ADT^A01|ID1|P|2.5`,
			seps:  hl7.Separators{Field: "|", Component: "^", Repetition: "~", Escape: `\`, Subcomponent: "&"},
			issue: hl7.IssueNoEncodingCharacters,
		},
		{
			name: "a separator that repeats another is not split on",
			// MSH-2 declares "^" as both the component and the repetition
			// separator. One character cannot mean two things, so the second
			// claim is dropped rather than honored.
			msg:   "MSH|^^\\&|A|B|C|D|20240101||ADT^A01|ID1|P|2.5\rPID|1||one~two",
			seps:  hl7.Separators{Field: "|", Component: "^", Repetition: "", Escape: `\`, Subcomponent: "&"},
			issue: hl7.IssueDuplicateSeparator,
			check: func(t *testing.T, m *hl7.Message) {
				// The component separator, claimed first, still works.
				if got := m.Value("MSH-9.2"); got != "A01" {
					t.Errorf("MSH-9.2 = %q, want A01", got)
				}
				// Nothing is split on the dropped one, so a tilde is just a
				// character rather than a delimiter nobody declared.
				if got := m.Value("PID-3"); got != "one~two" {
					t.Errorf("PID-3 = %q, want the unsplit one~two", got)
				}
				if m.Segment("PID", 1).Field(3).Repeats() {
					t.Error("PID-3 was split on a separator that was dropped")
				}
			},
		},
		{
			name:  "a header with nothing after the id falls back to the conventional set",
			msg:   "MSH",
			seps:  hl7.DefaultSeparators(),
			issue: hl7.IssueNoEncodingCharacters,
		},
		{
			name:  "an MSH that is not first is used anyway and flagged",
			msg:   "PID|1||MRN\rMSH|^~\\&|A|B|C|D|20240101||ADT^A01|ID1|P|2.5",
			seps:  hl7.DefaultSeparators(),
			issue: hl7.IssueHeaderNotFirst,
			check: func(t *testing.T, m *hl7.Message) {
				if m.Header.ControlID != "ID1" {
					t.Errorf("control id = %q, want ID1 - a late MSH is still the header", m.Header.ControlID)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := hl7.ParseString(tc.msg)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if m.Separators != tc.seps {
				t.Errorf("separators = %+v, want %+v", m.Separators, tc.seps)
			}
			if !m.HasIssue(tc.issue) {
				t.Errorf("issues %+v do not include %q", m.Issues, tc.issue)
			}
			if tc.check != nil {
				tc.check(t, m)
			}
		})
	}
}

func TestEscapes(t *testing.T) {
	// Every case is one NTE-3 value, so the table reads as "this on the wire
	// means this on the screen".
	tests := []struct {
		name string
		wire string
		want string
	}{
		{"field separator", `a\F\b`, "a|b"},
		{"component separator", `a\S\b`, "a^b"},
		{"subcomponent separator", `a\T\b`, "a&b"},
		{"repetition separator", `a\R\b`, "a~b"},
		{"escape character", `a\E\b`, `a\b`},
		{"line break", `line one\.br\line two`, "line one\nline two"},
		{"hex", `tab\X09\here`, "tab\there"},
		{"several in a row", `\F\\S\\T\`, "|^&"},
		// Anything the standard leaves to the sender stays exactly as sent:
		// decoding it wrongly would be inventing content.
		{"unknown escape is left alone", `a\Z99\b`, `a\Z99\b`},
		{"highlighting is left alone", `\H\urgent\N\`, `\H\urgent\N\`},
		{"bad hex is left alone", `\Xzz\`, `\Xzz\`},
		// An unterminated escape must not swallow the rest of the value.
		{"unterminated escape is literal", `50% off \ today`, `50% off \ today`},
		{"trailing escape character", `ends with \`, `ends with \`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := hl7.ParseString(`MSH|^~\&|A|B|C|D|20240101||ADT^A01|ID1|P|2.5` + "\r" + "NTE|1||" + tc.wire)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := m.Value("NTE-3"); got != tc.want {
				t.Errorf("NTE-3 = %q, want %q", got, tc.want)
			}
		})
	}
}

// Escape sequences expand to the separators the message declared, not to the
// conventional ones - which is the whole reason they exist.
func TestEscapesUseTheDeclaredSeparators(t *testing.T) {
	m := parseFixture(t, "custom_separators.hl7")
	if got := m.Value("NTE-3"); !strings.Contains(got, "Sent ! separated") {
		t.Errorf("NTE-3 = %q, want $F$ expanded to the declared field separator !", got)
	}
}

func TestParseEdgeCases(t *testing.T) {
	t.Run("empty input is the one failure", func(t *testing.T) {
		for _, in := range []string{"", "   ", "\r\n", "\r\r\r"} {
			if _, err := hl7.ParseString(in); !errors.Is(err, hl7.ErrEmpty) {
				t.Errorf("ParseString(%q) error = %v, want ErrEmpty", in, err)
			}
		}
	})

	t.Run("MLLP framing bytes are tolerated", func(t *testing.T) {
		framed := "\x0b" + `MSH|^~\&|A|B|C|D|20240101||ADT^A01|ID1|P|2.5` + "\r" + "\x1c\r"
		m, err := hl7.ParseString(framed)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(m.Segments) != 1 || m.Segments[0].ID != "MSH" {
			t.Fatalf("segments = %+v, want a single clean MSH", m.SegmentIDs())
		}
		if len(m.Issues) != 0 {
			t.Errorf("framing bytes produced issues: %+v", m.Issues)
		}
	})

	t.Run("a non-standard segment id is kept and flagged", func(t *testing.T) {
		m, err := hl7.ParseString(`MSH|^~\&|A|B|C|D|20240101||ADT^A01|ID1|P|2.5` + "\rZZZZ|1|kept")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if !m.HasIssue(hl7.IssueSegmentID) {
			t.Errorf("issues %+v do not flag the four-character id", m.Issues)
		}
		if got := m.Value("ZZZZ-2"); got != "kept" {
			t.Errorf("ZZZZ-2 = %q, want the value kept anyway", got)
		}
	})

	t.Run("a segment id with nothing after it is flagged", func(t *testing.T) {
		m, err := hl7.ParseString(`MSH|^~\&|A|B|C|D|20240101||ADT^A01|ID1|P|2.5` + "\rEVN")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if !m.HasIssue(hl7.IssueNoFields) {
			t.Errorf("issues %+v do not flag the fieldless EVN", m.Issues)
		}
	})

	t.Run("empty and trailing fields keep their positions", func(t *testing.T) {
		m, err := hl7.ParseString(`MSH|^~\&|A|B|C|D|20240101||ADT^A01|ID1|P|2.5` + "\rPID|1||MRN|||")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		pid := m.Segment("PID", 1)
		if got := len(pid.Fields); got != 6 {
			t.Fatalf("PID has %d fields, want the 6 that were sent", got)
		}
		if !pid.Field(2).Empty() || !pid.Field(6).Empty() {
			t.Error("an empty field did not report itself empty")
		}
		if pid.Field(3).Value != "MRN" {
			t.Errorf("PID-3 = %q; an empty PID-2 shifted the positions", pid.Field(3).Value)
		}
	})

	t.Run("a message that is only a header is fine", func(t *testing.T) {
		m, err := hl7.ParseString(`MSH|^~\&|A|B|C|D|20240101||ACK^A01|ID1|P|2.5`)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if !m.HasHeader() {
			t.Error("HasHeader() is false for a message that is nothing but MSH")
		}
	})

	t.Run("a fragment reports that it has no header", func(t *testing.T) {
		m := parseFixture(t, "no_msh.hl7")
		if m.HasHeader() {
			t.Error("HasHeader() is true for a message with no MSH")
		}
	})
}

// MSH-1 and MSH-2 are the separators themselves and must never be split or
// unescaped by the very characters they declare.
func TestHeaderFieldsAreVerbatim(t *testing.T) {
	m := parseFixture(t, "adt_a01.hl7")
	msh := m.Segment("MSH", 1)
	if got := msh.Field(1).Value; got != "|" {
		t.Errorf("MSH-1 = %q, want the field separator itself", got)
	}
	f2 := msh.Field(2)
	if f2.Value != `^~\&` {
		t.Errorf("MSH-2 = %q, want the encoding characters verbatim", f2.Value)
	}
	if len(f2.Repetitions) != 1 || len(f2.Repetition(1).Components) != 1 {
		t.Errorf("MSH-2 was split into components by its own separators: %+v", f2)
	}
	if got := m.Separators.EncodingCharacters(); got != `^~\&` {
		t.Errorf("EncodingCharacters() = %q", got)
	}
}

func TestValuePaths(t *testing.T) {
	m := parseFixture(t, "adt_a01.hl7")
	tests := []struct{ path, want string }{
		{"PID-5", "DOE^JOHN^A^JR^DR^^L"},
		{"PID-5.1", "DOE"},
		{"pid-5.1", "DOE"},
		{"PID-3", "MRN12345^^^HOSP^MR~999887777^^^SSA^SS"},
		{"PID-3[2].1", "999887777"},
		{"PV1-3.4.2", "HOSPITAL MAIN"},
		{"MSH(1)-10", "MSG00001"},
		// Nothing there, and nothing invented.
		{"PID-99", ""},
		{"PID-3[9].1", ""},
		{"OBX-5", ""},
		{"MSH(2)-10", ""},
		// Malformed paths resolve to nothing rather than to something adjacent.
		{"", ""},
		{"PID", ""},
		{"PID-", ""},
		{"PID-x", ""},
		{"-5", ""},
		{"PID-5.", ""},
		{"PID-5.1.2.3", ""},
		{"PID-3[0].1", ""},
	}
	for _, tc := range tests {
		if got := m.Value(tc.path); got != tc.want {
			t.Errorf("Value(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestPreviewAndTitle(t *testing.T) {
	m := parseFixture(t, "adt_a01.hl7")
	if got := m.Title(); got != "ADT^A01 · MSG00001" {
		t.Errorf("Title() = %q", got)
	}
	if got := m.PatientName(); got != "DOE^JOHN^A^JR^DR^^L" {
		t.Errorf("PatientName() = %q", got)
	}
	if got := m.Preview(); !strings.HasPrefix(got, "DOE^JOHN") || !strings.Contains(got, "MSH EVN PID") {
		t.Errorf("Preview() = %q, want the patient and the segment outline", got)
	}

	oru := parseFixture(t, "oru_r01.hl7")
	if got := oru.Preview(); !strings.Contains(got, "OBX x3") {
		t.Errorf("Preview() = %q, want the repeated OBX run collapsed", got)
	}

	frag := parseFixture(t, "no_msh.hl7")
	if got := frag.Title(); got != "HL7 message" {
		t.Errorf("Title() of a headerless fragment = %q", got)
	}
}

// A message with trailing empty name components should not be shown as
// "DOE^JOHN^^^^^" in a list.
func TestPatientNameTrimsTrailingComponents(t *testing.T) {
	m, err := hl7.ParseString(`MSH|^~\&|A|B|C|D|20240101||ADT^A01|ID1|P|2.5` + "\rPID|1||MRN||DOE^JOHN^^^^^")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := m.PatientName(); got != "DOE^JOHN" {
		t.Errorf("PatientName() = %q, want DOE^JOHN", got)
	}
}
