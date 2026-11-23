package ptp4l

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/facebook/time/ptp/protocol"

	"github.com/thomas-schreck/gnss_ptp_exporter/internal/promtest"
)

// Values recorded with `pmc -u -b 0 -s /var/run/ptp4lro` from linuxptp 4.2
// running as grandmaster (clock identity replaced).
const clockID protocol.ClockIdentity = 0x001122fffe334455

func tlvHead(id protocol.ManagementID, size int) protocol.ManagementTLVHead {
	return protocol.ManagementTLVHead{
		TLVHead:      protocol.TLVHead{TLVType: protocol.TLVManagement, LengthField: uint16(size - 4)},
		ManagementID: id,
	}
}

func defaultDS() *protocol.DefaultDataSetTLV {
	return &protocol.DefaultDataSetTLV{
		ManagementTLVHead: tlvHead(protocol.IDDefaultDataSet, binary.Size(protocol.DefaultDataSetTLV{})),
		SoTSC:             1,
		NumberPorts:       1,
		Priority1:         128,
		ClockQuality:      protocol.ClockQuality{ClockClass: 6, ClockAccuracy: 0x21, OffsetScaledLogVariance: 0xffff},
		Priority2:         128,
		ClockIdentity:     clockID,
	}
}

func parentDS() *protocol.ParentDataSetTLV {
	return &protocol.ParentDataSetTLV{
		ManagementTLVHead:                     tlvHead(protocol.IDParentDataSet, binary.Size(protocol.ParentDataSetTLV{})),
		ParentPortIdentity:                    protocol.PortIdentity{ClockIdentity: clockID},
		ObservedParentOffsetScaledLogVariance: 0xffff,
		ObservedParentClockPhaseChangeRate:    0x7fffffff,
		GrandmasterPriority1:                  128,
		GrandmasterClockQuality:               protocol.ClockQuality{ClockClass: 6, ClockAccuracy: 0x21, OffsetScaledLogVariance: 0xffff},
		GrandmasterPriority2:                  128,
		GrandmasterIdentity:                   clockID,
	}
}

func currentDS() *protocol.CurrentDataSetTLV {
	return &protocol.CurrentDataSetTLV{ManagementTLVHead: tlvHead(protocol.IDCurrentDataSet, binary.Size(protocol.CurrentDataSetTLV{}))}
}

func timeStatus() *protocol.TimeStatusNPTLV {
	return &protocol.TimeStatusNPTLV{
		ManagementTLVHead: tlvHead(protocol.IDTimeStatusNP, binary.Size(protocol.TimeStatusNPTLV{})),
		GMIdentity:        clockID,
	}
}

func portDS(port uint16, state protocol.PortState) *PortDataSetTLV {
	return &PortDataSetTLV{
		ManagementTLVHead:   tlvHead(protocol.IDPortDataSet, binary.Size(PortDataSetTLV{})),
		PortIdentity:        protocol.PortIdentity{ClockIdentity: clockID, PortNumber: port},
		PortState:           state,
		LogAnnounceInterval: 1, AnnounceReceiptTimeout: 3, VersionNumber: 2,
	}
}

type request struct {
	head   protocol.ManagementMsgHead
	id     protocol.ManagementID
	tlvLen uint16
}

// fakePTP4L is an in-process ptp4l management socket.
type fakePTP4L struct {
	path string
	mu   sync.Mutex
	reqs []request
	// answer returns the datagrams to send for a request.
	answer func(r request) [][]byte
}

func newFake(t *testing.T, answer func(r request) [][]byte) *fakePTP4L {
	t.Helper()
	f := &fakePTP4L{path: filepath.Join(t.TempDir(), "ptp4lro"), answer: answer}
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
			var r request
			rd := bytes.NewReader(buf[:n])
			var th protocol.TLVHead
			if binary.Read(rd, binary.BigEndian, &r.head) != nil || binary.Read(rd, binary.BigEndian, &th) != nil ||
				binary.Read(rd, binary.BigEndian, &r.id) != nil {
				continue
			}
			r.tlvLen = th.LengthField
			f.mu.Lock()
			f.reqs = append(f.reqs, r)
			f.mu.Unlock()
			for _, d := range f.answer(r) {
				_, _ = conn.WriteToUnix(d, from)
			}
		}
	}()
	return f
}

func response(t testing.TB, r request, tlv protocol.ManagementTLV) []byte {
	t.Helper()
	m := &protocol.Management{
		ManagementMsgHead: protocol.ManagementMsgHead{
			Header: protocol.Header{
				SdoIDAndMsgType:    protocol.NewSdoIDAndMsgType(protocol.MessageManagement, 0),
				Version:            protocol.Version,
				SourcePortIdentity: protocol.PortIdentity{ClockIdentity: clockID},
				SequenceID:         r.head.SequenceID,
				LogMessageInterval: protocol.MgmtLogMessageInterval,
			},
			TargetPortIdentity: r.head.SourcePortIdentity,
			ActionField:        protocol.RESPONSE,
		},
		TLV: tlv,
	}
	b, err := m.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint16(b[2:4], uint16(len(b)))
	return b
}

func errorResponse(t testing.TB, r request, code protocol.ManagementErrorID) []byte {
	t.Helper()
	m := &protocol.ManagementMsgErrorStatus{
		ManagementMsgHead: protocol.ManagementMsgHead{
			Header: protocol.Header{
				SdoIDAndMsgType: protocol.NewSdoIDAndMsgType(protocol.MessageManagement, 0),
				Version:         protocol.Version,
				SequenceID:      r.head.SequenceID,
			},
			ActionField: protocol.RESPONSE,
		},
		ManagementErrorStatusTLV: protocol.ManagementErrorStatusTLV{
			TLVHead:           protocol.TLVHead{TLVType: protocol.TLVManagementErrorStatus, LengthField: 8},
			ManagementErrorID: code,
			ManagementID:      r.id,
		},
	}
	b, err := m.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func grandmaster(t testing.TB) func(r request) [][]byte {
	return func(r request) [][]byte {
		var tlv protocol.ManagementTLV
		switch r.id {
		case protocol.IDDefaultDataSet:
			tlv = defaultDS()
		case protocol.IDParentDataSet:
			tlv = parentDS()
		case protocol.IDCurrentDataSet:
			tlv = currentDS()
		case protocol.IDTimeStatusNP:
			tlv = timeStatus()
		case protocol.IDPortDataSet:
			tlv = portDS(r.head.TargetPortIdentity.PortNumber, protocol.PortStateMaster)
		default:
			return [][]byte{errorResponse(t, r, protocol.ErrorNotSupported)}
		}
		return [][]byte{response(t, r, tlv)}
	}
}

func TestCollectorGrandmaster(t *testing.T) {
	f := newFake(t, grandmaster(t))
	c := New(f.path, 0, 2*time.Second, slog.New(slog.DiscardHandler))
	got := promtest.Gather(t, c)
	promtest.Expect(t, got, map[string]float64{
		"ptp4l_up":                             1,
		"ptp4l_grandmaster_role":               1,
		"ptp4l_clock_class":                    6,
		"ptp4l_clock_accuracy":                 0x21,
		"ptp4l_grandmaster_clock_class":        6,
		"ptp4l_steps_removed":                  0,
		"ptp4l_offset_from_master_nanoseconds": 0,
		"ptp4l_gm_present":                     0,
		`ptp4l_port_state{port="1"}`:           6,
		`ptp4l_clock_info{clock_identity="001122.fffe.334455",domain="0",grandmaster_identity="001122.fffe.334455"}`: 1,
	})

	// The exporter must never change ptp4l: every request is a GET with
	// an empty data field, like pmc sends.
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) != 5 {
		t.Errorf("%d requests, want 5", len(f.reqs))
	}
	for _, r := range f.reqs {
		if r.head.Action() != protocol.GET || r.tlvLen != 2 {
			t.Errorf("request %#x: action %d, TLV length %d", r.id, r.head.Action(), r.tlvLen)
		}
	}
	if last := f.reqs[len(f.reqs)-1]; last.id != protocol.IDPortDataSet || last.head.TargetPortIdentity.PortNumber != 1 {
		t.Errorf("last request %+v", last)
	}
}

func TestCollectorSlave(t *testing.T) {
	gm := grandmaster(t)
	f := newFake(t, func(r request) [][]byte {
		switch r.id {
		case protocol.IDParentDataSet:
			p := parentDS()
			p.GrandmasterIdentity = 0xaabbccfffe000001
			p.GrandmasterClockQuality.ClockClass = 7
			return [][]byte{response(t, r, p)}
		case protocol.IDCurrentDataSet:
			c := currentDS()
			c.StepsRemoved = 1
			c.OffsetFromMaster = protocol.NewTimeInterval(-12.5)
			c.MeanPathDelay = protocol.NewTimeInterval(800)
			return [][]byte{response(t, r, c)}
		case protocol.IDTimeStatusNP:
			ts := timeStatus()
			ts.GMPresent = 1
			return [][]byte{response(t, r, ts)}
		case protocol.IDPortDataSet:
			return [][]byte{response(t, r, portDS(1, protocol.PortStateSlave))}
		}
		return gm(r)
	})
	got := promtest.Gather(t, New(f.path, 0, 2*time.Second, slog.New(slog.DiscardHandler)))
	promtest.Expect(t, got, map[string]float64{
		"ptp4l_up":                             1,
		"ptp4l_grandmaster_role":               0,
		"ptp4l_grandmaster_clock_class":        7,
		"ptp4l_steps_removed":                  1,
		"ptp4l_offset_from_master_nanoseconds": -12.5,
		"ptp4l_mean_path_delay_nanoseconds":    800,
		"ptp4l_gm_present":                     1,
		`ptp4l_port_state{port="1"}`:           9,
	})
}

func TestSkipsStaleAndForeignDatagrams(t *testing.T) {
	gm := grandmaster(t)
	f := newFake(t, func(r request) [][]byte {
		stale := r
		stale.head.SequenceID--
		other := r
		other.id = protocol.IDClockAccuracy
		return append([][]byte{
			response(t, stale, defaultDS()),
			[]byte("garbage"),
			response(t, other, &protocol.ClockAccuracyTLV{ManagementTLVHead: tlvHead(protocol.IDClockAccuracy, 8)}),
		}, gm(r)...)
	})
	got := promtest.Gather(t, New(f.path, 0, 2*time.Second, slog.New(slog.DiscardHandler)))
	// A garbage datagram is an error (ptp4l never sends one); the stale
	// and foreign answers before it are skipped.
	promtest.Expect(t, got, map[string]float64{"ptp4l_up": 0, "ptp4l_grandmaster_role": 0})

	f2 := newFake(t, func(r request) [][]byte {
		stale := r
		stale.head.SequenceID += 100
		return append([][]byte{response(t, stale, defaultDS())}, gm(r)...)
	})
	got = promtest.Gather(t, New(f2.path, 0, 2*time.Second, slog.New(slog.DiscardHandler)))
	promtest.Expect(t, got, map[string]float64{"ptp4l_up": 1, `ptp4l_port_state{port="1"}`: 6})
}

func TestErrorStatus(t *testing.T) {
	gm := grandmaster(t)
	f := newFake(t, func(r request) [][]byte {
		if r.id == protocol.IDTimeStatusNP {
			return [][]byte{errorResponse(t, r, protocol.ErrorNotSupported)}
		}
		if r.id == protocol.IDPortDataSet {
			return [][]byte{errorResponse(t, r, protocol.ErrorNotSupported)}
		}
		return gm(r)
	})
	_, err := Query(f.path, 0, time.Second)
	var es *ErrorStatus
	if err == nil || !errors.As(err, &es) || es.ID != protocol.IDPortDataSet {
		t.Fatalf("err %v", err)
	}
}

func TestDown(t *testing.T) {
	c := New(filepath.Join(t.TempDir(), "nope"), 0, 200*time.Millisecond, slog.New(slog.DiscardHandler))
	got := promtest.Gather(t, c)
	promtest.Expect(t, got, map[string]float64{"ptp4l_up": 0, "ptp4l_grandmaster_role": 0})
	promtest.Absent(t, got, "ptp4l_clock_class")

	// A socket that never answers runs into the timeout.
	f := newFake(t, func(request) [][]byte { return nil })
	start := time.Now()
	if _, err := Query(f.path, 0, 200*time.Millisecond); err == nil {
		t.Fatal("no error from a silent ptp4l")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("timeout took %v", d)
	}
}

func TestGetRequestIsGet(t *testing.T) {
	b, err := GetRequest(protocol.IDDefaultDataSet, AllPorts, 24, 7).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 54 || int(binary.BigEndian.Uint16(b[2:4])) != len(b) {
		t.Fatalf("length %d, header says %d", len(b), binary.BigEndian.Uint16(b[2:4]))
	}
	if b[4] != 24 || b[46]&0x0f != byte(protocol.GET) {
		t.Errorf("domain %d action %d", b[4], b[46])
	}
}

func FuzzMatchResponse(f *testing.F) {
	r := request{head: GetRequest(protocol.IDPortDataSet, 1, 0, 3).ManagementMsgHead, id: protocol.IDPortDataSet}
	f.Add(response(f, r, portDS(1, protocol.PortStateMaster)))
	r.id = protocol.IDDefaultDataSet
	f.Add(response(f, r, defaultDS()))
	f.Add(errorResponse(f, r, protocol.ErrorNotSupported))
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, id := range []protocol.ManagementID{protocol.IDDefaultDataSet, protocol.IDParentDataSet,
			protocol.IDCurrentDataSet, protocol.IDTimeStatusNP, protocol.IDPortDataSet} {
			tlv, err := MatchResponse(b, id, 3)
			if err == nil && tlv.MgmtID() != id {
				t.Fatalf("got TLV %#x for %#x", tlv.MgmtID(), id)
			}
		}
		_, _ = DecodePortDataSet(b)
	})
}
