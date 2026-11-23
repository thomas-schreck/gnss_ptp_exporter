package gmsettings

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/facebook/time/ptp/protocol"

	"github.com/thomas-schreck/gnss_ptp_exporter/internal/ptp4l"
)

// Recorded on ts-time (linuxptp 4.2, grandmaster) with a GET on the
// read-only socket /var/run/ptp4lro, clock identity replaced by
// 001122fffe334455. pmc -u -b 0 -s /var/run/ptp4lro printed for it:
//
//	clockClass 6, clockAccuracy 0x21, offsetScaledLogVariance 0xffff,
//	currentUtcOffset 37, leap61 0, leap59 0, currentUtcOffsetValid 1,
//	ptpTimescale 1, timeTraceable 1, frequencyTraceable 1, timeSource 0x20
const (
	recordedGet      = "0d120036000000000000000000000000000000000000000000000000fe4e0001007fffffffffffffffffffff0000000000010002c001"
	recordedResponse = "0d12003e00000000000000000000000000000000001122fffe33445500000001007f0000000000000000fe4e000002000001000ac0010621ffff00253c20"
)

var recordedSettings = Settings{
	ClockClass: 6, ClockAccuracy: 0x21, OffsetScaledLogVariance: 0xffff, CurrentUTCOffset: 37,
	CurrentUTCOffsetValid: true, PTPTimescale: true, TimeTraceable: true, FrequencyTraceable: true,
	TimeSource: 0x20,
}

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeRecorded(t *testing.T) {
	// The same GET our ptp4l client builds (only the source port, the
	// PID, differs).
	get, err := ptp4l.GetRequest(ID, ptp4l.AllPorts, 0, 1).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	want := unhex(t, recordedGet)
	binary.BigEndian.PutUint16(want[28:30], binary.BigEndian.Uint16(get[28:30]))
	if !bytes.Equal(get, want) {
		t.Errorf("GET\n got %x\nwant %x", get, want)
	}

	tlv, err := ptp4l.MatchResponse(unhex(t, recordedResponse), ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	got := FromTLV(tlv.(*TLV))
	if got != recordedSettings {
		t.Errorf("decoded %+v", got)
	}
	if !strings.HasPrefix(got.PMCArgs(), "clockClass 6 clockAccuracy 0x21 offsetScaledLogVariance 0xffff currentUtcOffset 37 leap61 0") {
		t.Errorf("PMCArgs %q", got.PMCArgs())
	}
}

func TestSetRequestMatchesRecordedLayout(t *testing.T) {
	b, err := SetRequest(recordedSettings, 0, 9).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	rec := unhex(t, recordedResponse)
	if len(b) != 62 || int(binary.BigEndian.Uint16(b[2:4])) != len(b) {
		t.Fatalf("length %d, header %d", len(b), binary.BigEndian.Uint16(b[2:4]))
	}
	if b[46]&0x0f != byte(protocol.SET) || b[0]&0x0f != byte(protocol.MessageManagement) {
		t.Errorf("action %d type %d", b[46], b[0])
	}
	// Target: all clocks, all ports, as pmc sends.
	if !bytes.Equal(b[34:44], bytes.Repeat([]byte{0xff}, 10)) {
		t.Errorf("target %x", b[34:44])
	}
	// The TLV of a SET has the layout ptp4l answers a GET with.
	if !bytes.Equal(b[48:], rec[48:]) {
		t.Errorf("TLV\n got %x\nwant %x", b[48:], rec[48:])
	}
}

func TestFlags(t *testing.T) {
	for _, s := range []Settings{
		{Leap61: true}, {Leap59: true}, {CurrentUTCOffsetValid: true}, {PTPTimescale: true},
		{TimeTraceable: true}, {FrequencyTraceable: true}, {ClockClass: 248, ClockAccuracy: 0xfe, CurrentUTCOffset: -1},
	} {
		if got := FromTLV(s.TLV()); got != s {
			t.Errorf("round trip %+v -> %+v", s, got)
		}
	}
	if f := (Settings{Leap61: true, FrequencyTraceable: true}).TLV().TimeFlags; f != 0x21 {
		t.Errorf("flags %#x", f)
	}
	// Undefined bits are ignored.
	if got := FromTLV(&TLV{TimeFlags: 0xc0}); got != (Settings{}) {
		t.Errorf("%+v", got)
	}
}

func TestDecodeRejects(t *testing.T) {
	rec := unhex(t, recordedResponse)[48:]
	for name, b := range map[string][]byte{
		"short":      rec[:10],
		"length":     append(append([]byte{}, rec[:2]...), append([]byte{0, 12}, rec[4:]...)...),
		"other id":   append(append([]byte{}, rec[:4]...), append([]byte{0x20, 0x00}, rec[6:]...)...),
		"error type": append([]byte{0, 2}, rec[2:]...),
	} {
		if _, err := Decode(b); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// fakeRW is a ptp4l read-write socket that answers like ptp4l 4.2.
type fakeRW struct {
	path string
	got  chan []byte
}

func newFakeRW(t *testing.T, answer func(req []byte) [][]byte) *fakeRW {
	t.Helper()
	f := &fakeRW{path: filepath.Join(t.TempDir(), "ptp4l"), got: make(chan []byte, 10)}
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Net: "unixgram", Name: f.path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUnix(buf)
			if err != nil {
				return
			}
			req := append([]byte{}, buf[:n]...)
			f.got <- req
			for _, d := range answer(req) {
				_, _ = conn.WriteToUnix(d, from)
			}
		}
	}()
	return f
}

// respond turns a request into ptp4l's RESPONSE carrying tlv.
func respond(req, tlv []byte) []byte {
	b := append(append([]byte{}, req[:48]...), tlv...)
	binary.BigEndian.PutUint16(b[2:4], uint16(len(b)))
	b[46] = byte(protocol.RESPONSE)
	return b
}

func TestSet(t *testing.T) {
	want := recordedSettings
	want.ClockClass, want.ClockAccuracy = 7, 0xfe
	f := newFakeRW(t, func(req []byte) [][]byte {
		stale := respond(req, req[48:])
		stale[31]++ // other sequence number
		return [][]byte{stale, respond(req, req[48:])}
	})
	got, err := Set(f.path, 0, want, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("answer %+v", got)
	}
	req := <-f.got
	if len(req) != 62 || req[46] != byte(protocol.SET) || binary.BigEndian.Uint16(req[52:54]) != uint16(ID) {
		t.Errorf("request %x", req)
	}
	select {
	case r := <-f.got:
		t.Errorf("second request %x", r)
	default:
	}
}

func TestSetErrors(t *testing.T) {
	// ptp4l refuses with MANAGEMENT_ERROR_STATUS (e.g. the read-only
	// socket answers NOT_SUPPORTED).
	f := newFakeRW(t, func(req []byte) [][]byte {
		b := append([]byte{}, req[:48]...)
		b = append(b, 0, 2, 0, 8, 0, byte(protocol.ErrorNotSupported), 0xc0, 0x01, 0, 0, 0, 0)
		binary.BigEndian.PutUint16(b[2:4], uint16(len(b)))
		b[46] = byte(protocol.RESPONSE)
		return [][]byte{b}
	})
	_, err := Set(f.path, 0, recordedSettings, time.Second)
	var es *ptp4l.ErrorStatus
	if !errors.As(err, &es) || es.Code != protocol.ErrorNotSupported {
		t.Errorf("err %v", err)
	}
	silent := newFakeRW(t, func([]byte) [][]byte { return nil })
	if _, err := Set(silent.path, 0, recordedSettings, 100*time.Millisecond); err == nil {
		t.Error("no timeout")
	}
	if _, err := Set(filepath.Join(t.TempDir(), "nope"), 0, recordedSettings, time.Second); err == nil {
		t.Error("no error without socket")
	}
}

func FuzzDecode(f *testing.F) {
	rec, _ := hex.DecodeString(recordedResponse)
	f.Add(rec[48:])
	f.Add(rec)
	f.Fuzz(func(t *testing.T, b []byte) {
		tlv, err := Decode(b)
		if err != nil {
			return
		}
		g := tlv.(*TLV)
		if g.ManagementID != ID || g.LengthField != 10 {
			t.Fatalf("accepted %+v", g)
		}
		// What decodes encodes back to the same 14 bytes.
		out, err := g.MarshalBinary()
		if err != nil || !bytes.Equal(out, b[:14]) {
			t.Fatalf("re-encoded %x from %x (%v)", out, b, err)
		}
		_, _ = ptp4l.MatchResponse(b, ID, 1)
	})
}
