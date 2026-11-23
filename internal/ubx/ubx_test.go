package ubx

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thomas-schreck/gnss_ptp_exporter/internal/promtest"
)

// Payloads reconstructed field by field from `ubxtool -p MON-RF -p
// NAV-STATUS` on a ZED-F9T (TIM 2.25, protocol 29.25):
//
//	blockId 0 flags x0 antStatus 1 antPower 2 noisePerMS 98 agcCnt 3861 jamInd 14
//	blockId 1 flags x0 antStatus 1 antPower 2 noisePerMS 56 agcCnt 3159 jamInd 6
//	gpsFix 3 flags 0xdd fixStat 0x0 flags2 0x8 ttff 22860 msss 4294967295
func monRFPayload() []byte {
	p := []byte{0, 2, 0, 0}
	block := func(id, flags, antStatus, antPower byte, noise, agc uint16, jam byte) []byte {
		b := make([]byte, 24)
		b[0], b[1], b[2], b[3] = id, flags, antStatus, antPower
		binary.LittleEndian.PutUint16(b[12:], noise)
		binary.LittleEndian.PutUint16(b[14:], agc)
		b[16] = jam
		b[17], b[18], b[19], b[20] = 9, 165, 8, 161 // ofsI magI ofsQ magQ
		return b
	}
	p = append(p, block(0, 0x00, 1, 2, 98, 3861, 14)...)
	return append(p, block(1, 0x00, 1, 2, 56, 3159, 6)...)
}

func navStatusPayload() []byte {
	p := make([]byte, 16)
	binary.LittleEndian.PutUint32(p[0:], 148170000)
	p[4], p[5], p[6], p[7] = 3, 0xdd, 0x00, 0x08
	binary.LittleEndian.PutUint32(p[8:], 22860)
	binary.LittleEndian.PutUint32(p[12:], 4294967295)
	return p
}

func TestPollFrames(t *testing.T) {
	// Known poll frames, as ubxtool sends them.
	if got := hex.EncodeToString(Poll(ClassMON, IDMonRF)); got != "b5620a38000042d0" {
		t.Errorf("MON-RF poll %s", got)
	}
	if got := hex.EncodeToString(Poll(ClassNAV, IDNavStatus)); got != "b56201030000040d" {
		t.Errorf("NAV-STATUS poll %s", got)
	}
}

func TestParseMonRF(t *testing.T) {
	b, err := ParseMonRF(monRFPayload())
	if err != nil {
		t.Fatal(err)
	}
	want := []RFBlock{
		{BlockID: 0, AntStatus: 1, AntPower: 2, NoisePerMS: 98, AGCCnt: 3861, JamInd: 14},
		{BlockID: 1, AntStatus: 1, AntPower: 2, NoisePerMS: 56, AGCCnt: 3159, JamInd: 6},
	}
	if !slices.Equal(b, want) {
		t.Fatalf("got %+v", b)
	}
	if r := b[0].AGCRatio(); r < 0.4713 || r > 0.4715 {
		t.Errorf("AGC ratio %v", r)
	}
	p := monRFPayload()
	p[4+1] = 0x0e // jamming state critical (bits 0..1 = 2), other bits set
	b, _ = ParseMonRF(p)
	if b[0].JammingState != 2 {
		t.Errorf("jamming state %d", b[0].JammingState)
	}
	if _, err := ParseMonRF(monRFPayload()[:30]); err == nil {
		t.Error("short payload accepted")
	}
	p = monRFPayload()
	p[0] = 1
	if _, err := ParseMonRF(p); err == nil {
		t.Error("unknown version accepted")
	}
}

func TestParseNavStatus(t *testing.T) {
	n, err := ParseNavStatus(navStatusPayload())
	if err != nil {
		t.Fatal(err)
	}
	if n.SpoofDetState != 1 || n.GPSFix != 3 || n.Flags2 != 8 || n.TTFF != 22860 {
		t.Fatalf("got %+v", n)
	}
	for f2, want := range map[byte]uint8{0x00: 0, 0x10: 2, 0x18: 3, 0x1b: 3, 0x07: 0} {
		p := navStatusPayload()
		p[7] = f2
		if n, _ := ParseNavStatus(p); n.SpoofDetState != want {
			t.Errorf("flags2 %#x: spoof state %d, want %d", f2, n.SpoofDetState, want)
		}
	}
	if _, err := ParseNavStatus(make([]byte, 15)); err == nil {
		t.Error("short payload accepted")
	}
}

func TestScanner(t *testing.T) {
	rf := Encode(ClassMON, IDMonRF, monRFPayload())
	nav := Encode(ClassNAV, IDNavStatus, navStatusPayload())
	bad := Encode(ClassNAV, IDNavStatus, navStatusPayload())
	bad[len(bad)-1] ^= 0xff
	var stream []byte
	stream = append(stream, "$GNGGA,,,,,,0,00,99.99,,,,,,*56\r\n"...)
	stream = append(stream, 0xb5, 0x62, 0xff, 0xff) // stray sync with absurd length
	stream = append(stream, `{"class":"DEVICE","path":"/dev/ttyAMA0"}`+"\n"...)
	stream = append(stream, bad...)
	stream = append(stream, rf...)
	stream = append(stream, 0xb5) // split sync
	stream = append(stream, nav[1:]...)

	// Feed byte by byte to exercise partial frames.
	var s Scanner
	var got []Frame
	for i := range stream {
		_, _ = s.Write(stream[i : i+1])
		for {
			f, ok := s.Next()
			if !ok {
				break
			}
			got = append(got, f)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d frames: %+v", len(got), got)
	}
	if got[0].Class != ClassMON || got[1].Class != ClassNAV {
		t.Errorf("frames %x/%x", got[0].Class, got[1].Class)
	}
}

// fakeGPSD answers like gpsd 3.25 does for ubxtool and records the
// commands it receives.
type fakeGPSD struct {
	addr     string
	mu       sync.Mutex
	commands []string
}

func newFakeGPSD(t *testing.T, answerNav bool) *fakeGPSD {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	f := &fakeGPSD{addr: ln.Addr().String()}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn, answerNav)
		}
	}()
	return f
}

func (f *fakeGPSD) serve(conn net.Conn, answerNav bool) {
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, `{"class":"VERSION","release":"3.25","proto_major":3,"proto_minor":15}`+"\n")
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		f.mu.Lock()
		f.commands = append(f.commands, line)
		f.mu.Unlock()
		switch {
		case line == "?DEVICES;":
			_, _ = io.WriteString(conn, `{"class":"DEVICES","devices":[{"class":"DEVICE","path":"/dev/ttyAMA0","driver":"u-blox"}]}`+"\n")
		case strings.HasPrefix(line, "?WATCH="):
			_, _ = io.WriteString(conn, `{"class":"WATCH","enable":true,"raw":2}`+"\n$GNRMC,,V,,,,,,,,,,N*4D\r\n")
		case strings.Contains(line, `"hexdata":"b5620a38000042d0"`):
			_, _ = conn.Write(Encode(ClassMON, IDMonRF, monRFPayload()))
		case strings.Contains(line, `"hexdata":"b56201030000040d"`) && answerNav:
			_, _ = conn.Write(Encode(ClassNAV, IDNavStatus, navStatusPayload()))
		}
	}
}

func TestPollGPSD(t *testing.T) {
	f := newFakeGPSD(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r := PollGPSD(ctx, f.addr, "")
	if r.LastError != nil || !r.HaveRF || !r.HaveNav {
		t.Fatalf("poll: %+v", r)
	}
	if r.Device != "/dev/ttyAMA0" {
		t.Errorf("device %q", r.Device)
	}
	f.mu.Lock()
	cmds := slices.Clone(f.commands)
	f.mu.Unlock()
	// Only the device list, the watch and the two poll requests: nothing
	// that could change the receiver or gpsd configuration.
	want := []string{
		"?DEVICES;",
		`?WATCH={"device":"/dev/ttyAMA0","enable":true,"raw":2}`,
		`?DEVICE={"path":"/dev/ttyAMA0","hexdata":"b5620a38000042d0"}`,
		`?DEVICE={"path":"/dev/ttyAMA0","hexdata":"b56201030000040d"}`,
	}
	if !slices.Equal(cmds, want) {
		t.Errorf("commands:\n%s\nwant:\n%s", strings.Join(cmds, "\n"), strings.Join(want, "\n"))
	}

	got := promtest.Gather(t, &Collector{last: r})
	promtest.Expect(t, got, map[string]float64{
		"gnss_ubx_poll_success":                1,
		`gnss_rf_jamming_indicator{block="0"}`: 14,
		`gnss_rf_jamming_indicator{block="1"}`: 6,
		`gnss_rf_noise_per_ms{block="1"}`:      56,
		`gnss_rf_antenna_status{block="0"}`:    1,
		`gnss_rf_antenna_power{block="0"}`:     2,
		`gnss_rf_jamming_state{block="0"}`:     0,
		"gnss_spoofing_state":                  1,
	})
	if v := got[`gnss_rf_agc_ratio{block="0"}`]; v < 0.4713 || v > 0.4715 {
		t.Errorf("agc ratio %v", v)
	}
}

func TestPollGPSDPartial(t *testing.T) {
	f := newFakeGPSD(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r := PollGPSD(ctx, f.addr, "/dev/ttyAMA0")
	if r.LastError == nil || !r.HaveRF || r.HaveNav {
		t.Fatalf("poll: %+v", r)
	}
	if !strings.Contains(r.LastError.Error(), "NAV-STATUS") {
		t.Errorf("error %v", r.LastError)
	}
	got := promtest.Gather(t, &Collector{last: r})
	promtest.Expect(t, got, map[string]float64{"gnss_ubx_poll_success": 0, `gnss_rf_jamming_indicator{block="0"}`: 14})
	promtest.Absent(t, got, "gnss_spoofing_state")
}

func TestPollGPSDDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	r := PollGPSD(context.Background(), addr, "")
	if r.LastError == nil {
		t.Fatal("no error for closed port")
	}
	got := promtest.Gather(t, &Collector{})
	promtest.Expect(t, got, map[string]float64{"gnss_ubx_poll_success": 0})
}

func TestDeviceFromDevices(t *testing.T) {
	if _, ok, err := DeviceFromDevices([]byte(`{"class":"VERSION"}`)); ok || err != nil {
		t.Error("VERSION treated as device list")
	}
	if _, _, err := DeviceFromDevices([]byte(`{"class":"DEVICES","devices":[{"path":"/dev/ttyS0","driver":"NMEA0183"}]}`)); !errors.Is(err, ErrNoUblox) {
		t.Errorf("err %v", err)
	}
	d, ok, err := DeviceFromDevices([]byte(`{"class":"DEVICES","devices":[{"path":"/dev/ttyS0","driver":"NMEA0183"},{"path":"/dev/ttyACM0","driver":"u-blox"}]}`))
	if !ok || err != nil || d != "/dev/ttyACM0" {
		t.Errorf("got %q %v %v", d, ok, err)
	}
}

func FuzzScanner(f *testing.F) {
	f.Add(Encode(ClassMON, IDMonRF, monRFPayload()))
	f.Add(append(Encode(ClassNAV, IDNavStatus, navStatusPayload()), 0xb5))
	f.Add([]byte("$GNGGA,*00\r\n\xb5\x62\x0a\x38\x04\x00\x00\x01\x00\x00\x47\xc3"))
	f.Fuzz(func(t *testing.T, data []byte) {
		var s Scanner
		_, _ = s.Write(data)
		for {
			fr, ok := s.Next()
			if !ok {
				break
			}
			if len(fr.Payload) > MaxPayload {
				t.Fatalf("payload %d", len(fr.Payload))
			}
			var r Result
			r.apply(fr)
			r.At = time.Unix(1, 0)
			_ = promtest.Gather(t, &Collector{last: r})
		}
		if len(s.buf) > len(data) {
			t.Fatalf("buffer grew beyond input")
		}
	})
}

func FuzzParseMonRF(f *testing.F) {
	f.Add(monRFPayload())
	f.Add([]byte{0, 255, 0, 0})
	f.Fuzz(func(t *testing.T, p []byte) {
		b, err := ParseMonRF(p)
		if err == nil && len(b) != int(p[1]) {
			t.Fatalf("%d blocks for nBlocks %d", len(b), p[1])
		}
		for _, x := range b {
			if x.JammingState > 3 || x.AGCRatio() < 0 {
				t.Fatalf("bad block %+v", x)
			}
		}
	})
}

func FuzzParseNavStatus(f *testing.F) {
	f.Add(navStatusPayload())
	f.Fuzz(func(t *testing.T, p []byte) {
		n, err := ParseNavStatus(p)
		if err == nil && n.SpoofDetState > 3 {
			t.Fatalf("spoof state %d", n.SpoofDetState)
		}
	})
}
