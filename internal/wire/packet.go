// Package wire implements a pass-through MySQL proxy that decodes the client
// protocol as it forwards bytes, so the UI can show exactly what each client
// put on the socket and what the server said back.
package wire

import (
	"encoding/binary"

	"fmt"
	"io"
)

// Direction of a packet relative to the client.
type Direction string

const (
	ClientToServer Direction = "c2s"
	ServerToClient Direction = "s2c"
)

// maxPayload is the largest single MySQL packet payload (16 MiB - 1). Payloads
// at exactly this size mean the logical packet continues in the next one.
const maxPayload = 0xFFFFFF

// Packet is one raw MySQL protocol packet: a 4-byte header (3-byte
// little-endian payload length, 1-byte sequence id) followed by the payload.
type Packet struct {
	Seq     uint8
	Payload []byte
	// Raw is header+payload, forwarded verbatim to the peer.
	Raw []byte
}

// readPacket reads exactly one packet off the wire.
func readPacket(r io.Reader) (*Packet, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	n := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	raw := make([]byte, 4+n)
	copy(raw, header)
	if n > 0 {
		if _, err := io.ReadFull(r, raw[4:]); err != nil {
			return nil, err
		}
	}
	return &Packet{Seq: header[3], Payload: raw[4:], Raw: raw}, nil
}

// IsContinued reports whether this packet is part of a payload split across
// several packets because it exceeded the 16 MiB frame limit.
func (p *Packet) IsContinued() bool { return len(p.Payload) == maxPayload }

// reader is a cursor over a packet payload with the MySQL primitive types.
// Every accessor is bounds-checked and records the first failure, so callers
// can decode optimistically and check err once at the end.
type reader struct {
	b   []byte
	pos int
	err error
}

func newReader(b []byte) *reader { return &reader{b: b} }

func (r *reader) fail(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf(format, args...)
	}
}

func (r *reader) remaining() int {
	if r.pos > len(r.b) {
		return 0
	}
	return len(r.b) - r.pos
}

func (r *reader) byte() byte {
	if r.remaining() < 1 {
		r.fail("truncated: want 1 byte at %d", r.pos)
		return 0
	}
	v := r.b[r.pos]
	r.pos++
	return v
}

func (r *reader) fixed(n int) []byte {
	if r.remaining() < n {
		r.fail("truncated: want %d bytes at %d", n, r.pos)
		r.pos = len(r.b)
		return nil
	}
	v := r.b[r.pos : r.pos+n]
	r.pos += n
	return v
}

func (r *reader) uint16() uint16 {
	b := r.fixed(2)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

func (r *reader) uint32() uint32 {
	b := r.fixed(4)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

// lenEncInt reads a length-encoded integer. isNull is true only in row context,
// where the 0xFB prefix means SQL NULL.
func (r *reader) lenEncInt() (v uint64, isNull bool) {
	first := r.byte()
	switch {
	case first < 0xFB:
		return uint64(first), false
	case first == 0xFB:
		return 0, true
	case first == 0xFC:
		return uint64(r.uint16()), false
	case first == 0xFD:
		b := r.fixed(3)
		if b == nil {
			return 0, false
		}
		return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16, false
	default: // 0xFE
		b := r.fixed(8)
		if b == nil {
			return 0, false
		}
		return binary.LittleEndian.Uint64(b), false
	}
}

// lenEncString reads a length-encoded string.
func (r *reader) lenEncString() (s string, isNull bool) {
	n, null := r.lenEncInt()
	if null {
		return "", true
	}
	if n > uint64(r.remaining()) {
		r.fail("truncated string: want %d bytes, have %d", n, r.remaining())
		r.pos = len(r.b)
		return "", false
	}
	return string(r.fixed(int(n))), false
}

// nulString reads a NUL-terminated string.
func (r *reader) nulString() string {
	if r.pos > len(r.b) {
		r.fail("truncated: past end at %d", r.pos)
		return ""
	}
	for i := r.pos; i < len(r.b); i++ {
		if r.b[i] == 0 {
			s := string(r.b[r.pos:i])
			r.pos = i + 1
			return s
		}
	}
	// Unterminated: consume the rest. Some packets end without the NUL.
	s := string(r.b[r.pos:])
	r.pos = len(r.b)
	return s
}

// restOfPacket returns everything left as a string.
func (r *reader) restOfPacket() string {
	if r.remaining() <= 0 {
		return ""
	}
	s := string(r.b[r.pos:])
	r.pos = len(r.b)
	return s
}

func (r *reader) skip(n int) { r.fixed(n) }
