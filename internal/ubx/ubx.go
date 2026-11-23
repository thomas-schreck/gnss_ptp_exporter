// Package ubx decodes the u-blox UBX messages MON-RF and NAV-STATUS and
// polls them through gpsd's raw passthrough.
//
// The layouts follow the u-blox F9 interface descriptions (protocol 27 and
// later, e.g. ZED-F9T TIM 2.2x, ZED-F9P HPG 1.3x). Older generations
// (u-blox M8 and earlier) have no MON-RF; they answer the poll with
// nothing and the collector reports a failed poll.
package ubx

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Message classes and IDs.
const (
	ClassNAV = 0x01
	ClassMON = 0x0A

	IDNavStatus = 0x03
	IDMonRF     = 0x38
)

const (
	sync1      = 0xB5
	sync2      = 0x62
	headerLen  = 6 // sync(2) class id length(2)
	trailerLen = 2 // checksum
	// MaxPayload bounds the payload length accepted from the wire. The
	// largest message we decode is MON-RF with a handful of RF blocks.
	MaxPayload = 4096
)

// Frame is one checksummed UBX message.
type Frame struct {
	Class   byte
	ID      byte
	Payload []byte
}

// Checksum computes the 8-bit Fletcher checksum over class, id, length and
// payload, as defined by u-blox.
func Checksum(b []byte) (byte, byte) {
	var a, c byte
	for _, x := range b {
		a += x
		c += a
	}
	return a, c
}

// Encode builds a UBX frame.
func Encode(class, id byte, payload []byte) []byte {
	out := make([]byte, 0, headerLen+len(payload)+trailerLen)
	out = append(out, sync1, sync2, class, id)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(payload))) //nolint:gosec // callers pass small payloads
	out = append(out, payload...)
	a, b := Checksum(out[2:])
	return append(out, a, b)
}

// Poll returns the poll request (empty payload) for a message.
func Poll(class, id byte) []byte { return Encode(class, id, nil) }

// Scanner extracts UBX frames from a byte stream that may also carry NMEA
// sentences, RTCM or gpsd JSON. Bytes that do not form a valid frame are
// skipped.
type Scanner struct {
	buf []byte
}

// Write appends stream data. It never fails.
func (s *Scanner) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	return len(p), nil
}

// Next returns the next complete, valid frame, or ok=false if more data is
// needed.
func (s *Scanner) Next() (Frame, bool) {
	for {
		i := indexSync(s.buf)
		if i < 0 {
			// Keep a trailing 0xB5: it may start a frame in the next write.
			if n := len(s.buf); n > 0 && s.buf[n-1] == sync1 {
				s.buf = append(s.buf[:0], sync1)
			} else {
				s.buf = s.buf[:0]
			}
			return Frame{}, false
		}
		s.buf = s.buf[i:]
		if len(s.buf) < headerLen {
			return Frame{}, false
		}
		n := int(binary.LittleEndian.Uint16(s.buf[4:6]))
		if n > MaxPayload {
			s.buf = s.buf[2:] // not a real frame start
			continue
		}
		total := headerLen + n + trailerLen
		if len(s.buf) < total {
			return Frame{}, false
		}
		a, b := Checksum(s.buf[2 : headerLen+n])
		if a != s.buf[headerLen+n] || b != s.buf[headerLen+n+1] {
			s.buf = s.buf[2:]
			continue
		}
		f := Frame{Class: s.buf[2], ID: s.buf[3], Payload: append([]byte(nil), s.buf[headerLen:headerLen+n]...)}
		s.buf = s.buf[total:]
		return f, true
	}
}

func indexSync(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == sync1 && b[i+1] == sync2 {
			return i
		}
	}
	return -1
}

// RFBlock is one RF block of UBX-MON-RF (block 0 is usually L1, 1 is L2/L5).
type RFBlock struct {
	BlockID      uint8
	JammingState uint8 // flags bits 0..1: 0 unknown/disabled, 1 ok, 2 warning, 3 critical
	AntStatus    uint8 // 0 INIT, 1 DONTKNOW, 2 OK, 3 SHORT, 4 OPEN
	AntPower     uint8 // 0 OFF, 1 ON, 2 DONTKNOW
	PostStatus   uint32
	NoisePerMS   uint16
	AGCCnt       uint16 // 0..8191
	JamInd       uint8  // 0 no CW jamming .. 255 strong CW jamming
}

// AGCRatio returns the AGC monitor as a fraction of its full scale.
func (b RFBlock) AGCRatio() float64 { return float64(b.AGCCnt) / 8191 }

// ErrShort is returned for payloads that are shorter than their layout.
var ErrShort = errors.New("ubx: payload too short")

const (
	monRFHeader = 4
	monRFBlock  = 24
)

// ParseMonRF decodes a UBX-MON-RF payload.
func ParseMonRF(p []byte) ([]RFBlock, error) {
	if len(p) < monRFHeader {
		return nil, ErrShort
	}
	if p[0] != 0 {
		return nil, fmt.Errorf("ubx: MON-RF version %d not supported", p[0])
	}
	n := int(p[1])
	if len(p) < monRFHeader+n*monRFBlock {
		return nil, ErrShort
	}
	out := make([]RFBlock, 0, n)
	for i := 0; i < n; i++ {
		b := p[monRFHeader+i*monRFBlock:]
		out = append(out, RFBlock{
			BlockID:      b[0],
			JammingState: b[1] & 0x03,
			AntStatus:    b[2],
			AntPower:     b[3],
			PostStatus:   binary.LittleEndian.Uint32(b[4:8]),
			NoisePerMS:   binary.LittleEndian.Uint16(b[12:14]),
			AGCCnt:       binary.LittleEndian.Uint16(b[14:16]),
			JamInd:       b[16],
		})
	}
	return out, nil
}

// NavStatus holds the fields of UBX-NAV-STATUS this exporter uses.
type NavStatus struct {
	GPSFix        uint8
	Flags         uint8
	FixStat       uint8
	Flags2        uint8
	SpoofDetState uint8 // flags2 bits 3..4: 0 unknown/off, 1 no spoofing, 2 indicated, 3 multiple indications
	TTFF          uint32
	MSSS          uint32
}

// ParseNavStatus decodes a UBX-NAV-STATUS payload (16 bytes).
func ParseNavStatus(p []byte) (NavStatus, error) {
	if len(p) < 16 {
		return NavStatus{}, ErrShort
	}
	return NavStatus{
		GPSFix:        p[4],
		Flags:         p[5],
		FixStat:       p[6],
		Flags2:        p[7],
		SpoofDetState: (p[7] >> 3) & 0x03,
		TTFF:          binary.LittleEndian.Uint32(p[8:12]),
		MSSS:          binary.LittleEndian.Uint32(p[12:16]),
	}, nil
}
