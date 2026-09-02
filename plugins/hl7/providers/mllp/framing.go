package mllp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// MLLP's three control bytes: 0x0B opens a block, 0x1C 0x0D closes it. HL7
// itself never uses 0x0B or 0x1C, so there is nothing to escape.
const (
	startByte = 0x0B
	endByte1  = 0x1C
	endByte2  = '\r' // 0x0D
)

// ErrFrameTooLarge is returned by frameReader.next when a frame's payload
// exceeds the configured limit without a trailer ever arriving. The
// connection this came from is expected to be closed rather than read
// further: a frame that never terminates must be bounded, not buffered
// forever.
var ErrFrameTooLarge = errors.New("mllp: frame exceeds the configured maximum without a trailer")

// errMidFrame wraps io.ErrUnexpectedEOF for a connection that closed after a
// start byte but before a complete trailer - distinct from a clean
// between-frames close, which frameReader.next reports as io.EOF.
var errMidFrame = fmt.Errorf("mllp: connection closed mid-frame: %w", io.ErrUnexpectedEOF)

// frameReader pulls one MLLP-framed message at a time off a byte stream.
//
// It is built on bufio.Reader precisely because that is what makes it
// transport-agnostic about how the bytes arrived: ReadByte blocks on the
// underlying reader for more data mid-frame, which is what makes a message
// split across several TCP packets (and therefore several Read calls) just
// work, and bufio.Reader's own internal buffer holds whatever came after one
// frame's trailer for the next call to next(), which is what makes several
// pipelined messages - arriving back to back, possibly within a single
// underlying Read - just work too. Neither case needs any code of its own
// here; both fall out of using a buffered reader instead of assuming one
// Read is one message.
type frameReader struct {
	r       *bufio.Reader
	maxSize int
}

// newFrameReader wraps r. maxSize <= 0 means no limit, which nothing in this
// package ever passes - see DefaultMaxMessageBytes.
func newFrameReader(r io.Reader, maxSize int) *frameReader {
	return &frameReader{r: bufio.NewReader(r), maxSize: maxSize}
}

// next reads and returns the payload of the next frame, with the framing
// bytes stripped.
//
// Bytes before the first 0x0B - and, since the loop that finds a start byte
// runs again after every trailer, bytes between one frame's trailer and the
// next frame's start byte too - are silently discarded rather than treated
// as an error: a stray byte an upstream hop injected is not a reason to fail
// a connection that is otherwise sending well-formed frames.
//
// It returns io.EOF when the connection closed cleanly between frames (nothing
// read yet, or only discarded junk), errMidFrame when it closed after a start
// byte but before a complete trailer, and ErrFrameTooLarge when maxSize is
// exceeded before a trailer arrives - the caller is expected to close the
// connection in every error case.
func (fr *frameReader) next() ([]byte, error) {
	for {
		b, err := fr.r.ReadByte()
		if err != nil {
			return nil, err // clean EOF (or a read error) between frames
		}
		if b == startByte {
			break
		}
		// Anything else here is junk before the header: discard and keep
		// looking.
	}

	var buf []byte
	for {
		b, err := fr.r.ReadByte()
		if err != nil {
			return nil, midFrameErr(err)
		}
		if b == endByte1 {
			b2, err := fr.r.ReadByte()
			if err != nil {
				return nil, midFrameErr(err)
			}
			if b2 == endByte2 {
				return buf, nil
			}
			// 0x1C not immediately followed by \r is not a trailer. HL7
			// does not otherwise use either byte, so this should not
			// happen on real traffic, but treating it as ordinary payload
			// rather than silently truncating the message is the safer
			// failure mode for a capture tool.
			buf = append(buf, b, b2)
		} else {
			buf = append(buf, b)
		}
		if fr.maxSize > 0 && len(buf) > fr.maxSize {
			return nil, ErrFrameTooLarge
		}
	}
}

func midFrameErr(err error) error {
	if errors.Is(err, io.EOF) {
		return errMidFrame
	}
	return err
}

// frame wraps payload in MLLP's control bytes for transmission.
func frame(payload []byte) []byte {
	out := make([]byte, 0, len(payload)+3)
	out = append(out, startByte)
	out = append(out, payload...)
	out = append(out, endByte1, endByte2)
	return out
}
