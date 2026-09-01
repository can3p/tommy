package sms

// Segment arithmetic: the part of SMS people actually get wrong, and the reason
// a message looks fine in a test and costs three segments in production.
//
// A GSM-7 message is packed seven bits per character, so 140 octets hold 160
// characters. Nine characters (^ { } \ [ ~ ] | and the euro sign, plus form
// feed) are not in the basic alphabet: they are sent as an escape followed by a
// second septet, so each costs two. One character outside GSM-7 entirely - a
// curly quote pasted from a word processor, an emoji - forces the whole message
// to UCS-2, where 140 octets hold only 70 UTF-16 code units.
//
// Concatenating messages spends six octets of every segment on the UDH that
// says "part 2 of 3", which leaves 153 septets or 67 code units per segment.
// An escape pair and a surrogate pair are indivisible, so a segment boundary
// that would split one leaves that slot empty instead: packing is greedy rather
// than a plain division.

// Encoding is the character encoding a body has to be sent in.
type Encoding string

// The encodings a message can use.
const (
	// GSM7 is the 7-bit default alphabet of GSM 03.38.
	GSM7 Encoding = "GSM-7"
	// UCS2 is the 16-bit fallback used as soon as one character is not GSM-7.
	UCS2 Encoding = "UCS-2"
)

// Segment capacities, in encoding units (septets for GSM-7, UTF-16 code units
// for UCS-2). The single-segment limits are larger because a lone segment
// carries no concatenation header.
const (
	GSM7SingleLimit = 160
	GSM7MultiLimit  = 153
	UCS2SingleLimit = 70
	UCS2MultiLimit  = 67
)

// Segments is how a body is chopped onto the wire, and what the UI shows in a
// badge next to every message.
type Segments struct {
	// Count is how many segments the body needs. An empty body still costs one,
	// which is what carriers bill.
	Count int `json:"count"`
	// Encoding is the alphabet the whole body has to be sent in.
	Encoding Encoding `json:"encoding"`
	// Units is the total cost of the body: septets under GSM-7, UTF-16 code
	// units under UCS-2. Escape pairs and surrogate pairs count as two.
	Units int `json:"units"`
	// Capacity is how many units fit in each segment at this Count, so
	// Units/Capacity is comparable to what a provider reports.
	Capacity int `json:"capacity"`
	// Remaining is how many units are still free in the last segment.
	Remaining int `json:"remaining"`
}

// gsm7Basic is the GSM 03.38 default alphabet. Position 0x1B is the escape and
// is not a character, so it is absent.
const gsm7Basic = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ" +
	" !\"#¤%&'()*+,-./0123456789:;<=>?" +
	"¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§" +
	"¿abcdefghijklmnopqrstuvwxyzäöñüà"

// gsm7Extension is the escape table: each of these costs two septets, because
// it is sent as ESC followed by the character.
const gsm7Extension = "\f^{}\\[~]|€"

var gsm7Cost = buildGSM7Table()

func buildGSM7Table() map[rune]int {
	m := make(map[rune]int, 128)
	for _, r := range gsm7Basic {
		m[r] = 1
	}
	for _, r := range gsm7Extension {
		m[r] = 2
	}
	return m
}

// EncodingOf reports which alphabet body has to be sent in: GSM-7 when every
// character is in the default or escape table, UCS-2 as soon as one is not.
func EncodingOf(body string) Encoding {
	for _, r := range body {
		if _, ok := gsm7Cost[r]; !ok {
			return UCS2
		}
	}
	return GSM7
}

// CountSegments computes the segmentation of a message body.
//
// It packs greedily rather than dividing, because the two-unit sequences - a
// GSM-7 escape pair, a UCS-2 surrogate pair - may not straddle a segment
// boundary. For a body made only of one-unit characters greedy packing is
// exactly ceil(units/capacity), which is the common case.
func CountSegments(body string) Segments {
	enc := EncodingOf(body)
	single, multi := GSM7SingleLimit, GSM7MultiLimit
	if enc == UCS2 {
		single, multi = UCS2SingleLimit, UCS2MultiLimit
	}

	costs := unitCosts(body, enc)
	total := 0
	for _, c := range costs {
		total += c
	}
	if total <= single {
		return Segments{
			Count:     1,
			Encoding:  enc,
			Units:     total,
			Capacity:  single,
			Remaining: single - total,
		}
	}

	count, used := 1, 0
	for _, c := range costs {
		if used+c > multi {
			count++
			used = 0
		}
		used += c
	}
	return Segments{
		Count:     count,
		Encoding:  enc,
		Units:     total,
		Capacity:  multi,
		Remaining: multi - used,
	}
}

// unitCosts is the per-character cost of body in the given encoding.
func unitCosts(body string, enc Encoding) []int {
	costs := make([]int, 0, len(body))
	for _, r := range body {
		switch {
		case enc == GSM7:
			costs = append(costs, gsm7Cost[r])
		case r > 0xFFFF:
			// Outside the basic multilingual plane: a UTF-16 surrogate pair.
			costs = append(costs, 2)
		default:
			costs = append(costs, 1)
		}
	}
	return costs
}
