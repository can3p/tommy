package sms_test

import (
	"strings"
	"testing"

	"github.com/can3p/tommy/plugins/sms"
)

// The segment arithmetic is the part of SMS people get wrong, so it gets the
// most thorough table in the package: both alphabets, both boundaries, the
// escape table, and the two cases where a pair may not be split across a
// segment and greedy packing therefore beats a plain division.
func TestCountSegments(t *testing.T) {
	const (
		curly = "’" // right single quote: BMP, but not in GSM-7
		emoji = "\U0001F600"
	)

	tests := []struct {
		name string
		body string
		want sms.Segments
	}{{
		name: "empty body still costs one segment",
		body: "",
		want: sms.Segments{Count: 1, Encoding: sms.GSM7, Units: 0, Capacity: 160, Remaining: 160},
	}, {
		name: "plain ascii",
		body: "Hello",
		want: sms.Segments{Count: 1, Encoding: sms.GSM7, Units: 5, Capacity: 160, Remaining: 155},
	}, {
		name: "gsm-7 single segment boundary at 160",
		body: strings.Repeat("a", 160),
		want: sms.Segments{Count: 1, Encoding: sms.GSM7, Units: 160, Capacity: 160, Remaining: 0},
	}, {
		name: "gsm-7 spills to two segments at 161",
		body: strings.Repeat("a", 161),
		want: sms.Segments{Count: 2, Encoding: sms.GSM7, Units: 161, Capacity: 153, Remaining: 145},
	}, {
		name: "gsm-7 exactly two concatenated segments",
		body: strings.Repeat("a", 306),
		want: sms.Segments{Count: 2, Encoding: sms.GSM7, Units: 306, Capacity: 153, Remaining: 0},
	}, {
		name: "gsm-7 spills to three segments at 307",
		body: strings.Repeat("a", 307),
		want: sms.Segments{Count: 3, Encoding: sms.GSM7, Units: 307, Capacity: 153, Remaining: 152},
	}, {
		name: "newline and carriage return are gsm-7",
		body: "a\r\nb",
		want: sms.Segments{Count: 1, Encoding: sms.GSM7, Units: 4, Capacity: 160, Remaining: 156},
	}, {
		name: "accented characters in the gsm-7 basic table cost one",
		body: "Où Ça? £100 Ω",
		want: sms.Segments{Count: 1, Encoding: sms.GSM7, Units: 13, Capacity: 160, Remaining: 147},
	}, {
		name: "a single extension character costs two septets",
		body: "€",
		want: sms.Segments{Count: 1, Encoding: sms.GSM7, Units: 2, Capacity: 160, Remaining: 158},
	}, {
		name: "the whole extension table costs two each",
		body: "^{}\\[~]|€\f",
		want: sms.Segments{Count: 1, Encoding: sms.GSM7, Units: 20, Capacity: 160, Remaining: 140},
	}, {
		name: "80 extension characters exactly fill one segment",
		body: strings.Repeat("€", 80),
		want: sms.Segments{Count: 1, Encoding: sms.GSM7, Units: 160, Capacity: 160, Remaining: 0},
	}, {
		name: "81 extension characters need two segments",
		body: strings.Repeat("€", 81),
		// 76 escape pairs fit in 153 septets with one septet wasted, so the
		// second segment carries the remaining five characters.
		want: sms.Segments{Count: 2, Encoding: sms.GSM7, Units: 162, Capacity: 153, Remaining: 143},
	}, {
		name: "an escape pair is never split across a segment boundary",
		// 306 septets would divide into exactly two segments, but only 76 pairs
		// fit in each, so the 153rd character needs a third segment.
		body: strings.Repeat("€", 153),
		want: sms.Segments{Count: 3, Encoding: sms.GSM7, Units: 306, Capacity: 153, Remaining: 151},
	}, {
		name: "one non-gsm character forces the whole message to ucs-2",
		body: "hello" + curly,
		want: sms.Segments{Count: 1, Encoding: sms.UCS2, Units: 6, Capacity: 70, Remaining: 64},
	}, {
		name: "lowercase c-cedilla is not in gsm-7",
		body: "ça",
		want: sms.Segments{Count: 1, Encoding: sms.UCS2, Units: 2, Capacity: 70, Remaining: 68},
	}, {
		name: "ucs-2 single segment boundary at 70",
		body: strings.Repeat(curly, 70),
		want: sms.Segments{Count: 1, Encoding: sms.UCS2, Units: 70, Capacity: 70, Remaining: 0},
	}, {
		name: "ucs-2 spills to two segments at 71",
		body: strings.Repeat(curly, 71),
		want: sms.Segments{Count: 2, Encoding: sms.UCS2, Units: 71, Capacity: 67, Remaining: 63},
	}, {
		name: "ucs-2 exactly two concatenated segments",
		body: strings.Repeat(curly, 134),
		want: sms.Segments{Count: 2, Encoding: sms.UCS2, Units: 134, Capacity: 67, Remaining: 0},
	}, {
		name: "an emoji forces ucs-2 and costs two code units",
		body: "hi " + emoji,
		want: sms.Segments{Count: 1, Encoding: sms.UCS2, Units: 5, Capacity: 70, Remaining: 65},
	}, {
		name: "35 emoji exactly fill a single ucs-2 segment",
		body: strings.Repeat(emoji, 35),
		want: sms.Segments{Count: 1, Encoding: sms.UCS2, Units: 70, Capacity: 70, Remaining: 0},
	}, {
		name: "36 emoji need two segments",
		body: strings.Repeat(emoji, 36),
		want: sms.Segments{Count: 2, Encoding: sms.UCS2, Units: 72, Capacity: 67, Remaining: 61},
	}, {
		name: "a surrogate pair is never split across a segment boundary",
		// 134 code units would divide into exactly two segments, but only 33
		// emoji fit in each, so the 67th emoji needs a third.
		body: strings.Repeat(emoji, 67),
		want: sms.Segments{Count: 3, Encoding: sms.UCS2, Units: 134, Capacity: 67, Remaining: 65},
	}, {
		name: "a mixed body pays for each escape pair",
		body: "cost: 50€ or 60€",
		want: sms.Segments{Count: 1, Encoding: sms.GSM7, Units: 18, Capacity: 160, Remaining: 142},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sms.CountSegments(tt.body)
			if got != tt.want {
				t.Errorf("CountSegments(%q):\n got %#v\nwant %#v", truncateForMsg(tt.body), got, tt.want)
			}
			if got.Count > 1 && got.Units <= got.Capacity {
				t.Errorf("multi-segment result claims %d units in a %d-unit capacity", got.Units, got.Capacity)
			}
			if got.Remaining < 0 || got.Remaining >= got.Capacity && got.Units > 0 {
				t.Errorf("Remaining %d is not inside [0,%d)", got.Remaining, got.Capacity)
			}
		})
	}
}

// Greedy packing must never be cheaper than the theoretical minimum, and never
// more than one segment worse than it.
func TestCountSegmentsAgainstNaiveDivision(t *testing.T) {
	bodies := map[string]string{
		"ascii":     strings.Repeat("a", 1000),
		"extension": strings.Repeat("€", 400),
		"ucs2":      strings.Repeat("’", 400),
		"emoji":     strings.Repeat("\U0001F600", 200),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			got := sms.CountSegments(body)
			naive := (got.Units + got.Capacity - 1) / got.Capacity
			if got.Count < naive {
				t.Errorf("count %d is below the theoretical minimum %d", got.Count, naive)
			}
			if got.Count > naive+1 {
				t.Errorf("count %d wastes more than one segment over the minimum %d", got.Count, naive)
			}
		})
	}
}

func TestEncodingOf(t *testing.T) {
	tests := []struct {
		name string
		body string
		want sms.Encoding
	}{
		{"empty", "", sms.GSM7},
		{"ascii", "Hello, world! 123", sms.GSM7},
		{"gsm basic accents", "èéùìòÇØøÅåÆæßÉÄÖÑÜ§¿äöñüà", sms.GSM7},
		{"gsm greek capitals", "ΔΦΓΛΩΠΨΣΘΞ", sms.GSM7},
		{"gsm punctuation", "@£$¥¤¡§_", sms.GSM7},
		{"extension table stays gsm-7", "^{}\\[~]|€", sms.GSM7},
		{"curly quote is not gsm-7", "don’t", sms.UCS2},
		{"emoji is not gsm-7", "ok \U0001F44D", sms.UCS2},
		{"cyrillic is not gsm-7", "привет", sms.UCS2},
		{"lowercase cedilla is not gsm-7", "garçon", sms.UCS2},
		{"tab is not gsm-7", "a\tb", sms.UCS2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sms.EncodingOf(tt.body); got != tt.want {
				t.Errorf("EncodingOf(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func truncateForMsg(s string) string {
	if len(s) <= 40 {
		return s
	}
	return s[:40] + "..."
}
