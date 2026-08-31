package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ByteSize is a byte count that accepts either a TOML integer (`1024`) or a
// human string (`"256MB"`, `"1.5 GiB"`).
type ByteSize int64

var unitMultipliers = []struct {
	suffix string
	mult   int64
}{
	{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"TIB", 1 << 40},
	{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"TB", 1000 * 1000 * 1000 * 1000},
	{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
	{"B", 1},
}

// ParseByteSize parses a human byte size. Bare numbers are bytes.
func ParseByteSize(s string) (ByteSize, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, fmt.Errorf("empty byte size")
	}
	upper := strings.ToUpper(trimmed)
	for _, u := range unitMultipliers {
		if !strings.HasSuffix(upper, u.suffix) {
			continue
		}
		num := strings.TrimSpace(upper[:len(upper)-len(u.suffix)])
		if num == "" {
			return 0, fmt.Errorf("byte size %q has no number", s)
		}
		f, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return 0, fmt.Errorf("byte size %q: %w", s, err)
		}
		if f < 0 {
			return 0, fmt.Errorf("byte size %q is negative", s)
		}
		return ByteSize(f * float64(u.mult)), nil
	}
	n, err := strconv.ParseInt(upper, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("byte size %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("byte size %q is negative", s)
	}
	return ByteSize(n), nil
}

// String renders the size back in a compact human form.
func (b ByteSize) String() string {
	n := int64(b)
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%dGiB", n/(1<<30))
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", n/(1<<20))
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%dKiB", n/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// Bytes returns the size as an int64.
func (b ByteSize) Bytes() int64 { return int64(b) }
