// Package gmsettings reads and writes linuxptp's GRANDMASTER_SETTINGS_NP
// management TLV (management ID 0xC001), the clock quality and time
// properties ptp4l announces as grandmaster.
//
// facebook/time does not know this linuxptp-specific TLV. The layout
// follows struct grandmaster_settings_np in linuxptp's tlv.h, the time
// flags follow msg.h, and the SET message is built like pmc's
// pmc_send_set_action (pmc_common.c).
//
// This package lives below cmd/gnss_ptp_clockclass, so Go's internal
// package rule keeps the exporter from importing it: SetRequest and Set
// are the only code in the repository that builds a management SET, and
// they can only send this one TLV.
package gmsettings

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/facebook/time/ptp/protocol"

	"github.com/thomas-schreck/gnss_ptp_exporter/internal/ptp4l"
)

// ID is the management ID of GRANDMASTER_SETTINGS_NP (MID_GRANDMASTER_SETTINGS_NP).
const ID protocol.ManagementID = 0xC001

// Time flags (linuxptp msg.h).
const (
	FlagLeap61             = 1 << 0
	FlagLeap59             = 1 << 1
	FlagUTCOffsetValid     = 1 << 2
	FlagPTPTimescale       = 1 << 3
	FlagTimeTraceable      = 1 << 4
	FlagFrequencyTraceable = 1 << 5
)

// dataSize is sizeof(struct grandmaster_settings_np): clockQuality (4),
// utc_offset (2), time_flags (1), time_source (1).
const dataSize = 8

// TLV is the GRANDMASTER_SETTINGS_NP management TLV on the wire.
type TLV struct {
	protocol.ManagementTLVHead

	ClockQuality protocol.ClockQuality
	UTCOffset    int16
	TimeFlags    uint8
	TimeSource   protocol.TimeSource
}

// MarshalBinary encodes the TLV, starting at the TLV type.
func (t *TLV) MarshalBinary() ([]byte, error) {
	var b bytes.Buffer
	err := binary.Write(&b, binary.BigEndian, t)
	return b.Bytes(), err
}

// Decode decodes a GRANDMASTER_SETTINGS_NP TLV starting at the TLV type.
// Like linuxptp it insists on the exact data length.
func Decode(data []byte) (protocol.ManagementTLV, error) {
	tlv := &TLV{}
	if err := binary.Read(bytes.NewReader(data), binary.BigEndian, tlv); err != nil {
		return nil, err
	}
	if tlv.TLVType != protocol.TLVManagement || tlv.ManagementID != ID {
		return nil, fmt.Errorf("not a GRANDMASTER_SETTINGS_NP TLV")
	}
	if tlv.LengthField != 2+dataSize {
		return nil, fmt.Errorf("GRANDMASTER_SETTINGS_NP: length %d, want %d", tlv.LengthField, 2+dataSize)
	}
	return tlv, nil
}

func init() {
	protocol.RegisterMgmtTLVDecoder(ID, Decode)
}

// Settings are the decoded GRANDMASTER_SETTINGS_NP values, named like
// pmc prints them.
type Settings struct {
	ClockClass              uint8
	ClockAccuracy           uint8
	OffsetScaledLogVariance uint16
	CurrentUTCOffset        int16
	Leap61                  bool
	Leap59                  bool
	CurrentUTCOffsetValid   bool
	PTPTimescale            bool
	TimeTraceable           bool
	FrequencyTraceable      bool
	TimeSource              uint8
}

// FromTLV converts the wire form. Undefined flag bits are dropped.
func FromTLV(t *TLV) Settings {
	f := t.TimeFlags
	return Settings{
		ClockClass:              uint8(t.ClockQuality.ClockClass),
		ClockAccuracy:           uint8(t.ClockQuality.ClockAccuracy),
		OffsetScaledLogVariance: t.ClockQuality.OffsetScaledLogVariance,
		CurrentUTCOffset:        t.UTCOffset,
		Leap61:                  f&FlagLeap61 != 0,
		Leap59:                  f&FlagLeap59 != 0,
		CurrentUTCOffsetValid:   f&FlagUTCOffsetValid != 0,
		PTPTimescale:            f&FlagPTPTimescale != 0,
		TimeTraceable:           f&FlagTimeTraceable != 0,
		FrequencyTraceable:      f&FlagFrequencyTraceable != 0,
		TimeSource:              uint8(t.TimeSource),
	}
}

// TLV converts the settings to the wire form.
func (s Settings) TLV() *TLV {
	var f uint8
	for _, b := range []struct {
		on   bool
		flag uint8
	}{
		{s.Leap61, FlagLeap61}, {s.Leap59, FlagLeap59},
		{s.CurrentUTCOffsetValid, FlagUTCOffsetValid}, {s.PTPTimescale, FlagPTPTimescale},
		{s.TimeTraceable, FlagTimeTraceable}, {s.FrequencyTraceable, FlagFrequencyTraceable},
	} {
		if b.on {
			f |= b.flag
		}
	}
	return &TLV{
		ManagementTLVHead: protocol.ManagementTLVHead{
			TLVHead:      protocol.TLVHead{TLVType: protocol.TLVManagement, LengthField: 2 + dataSize},
			ManagementID: ID,
		},
		ClockQuality: protocol.ClockQuality{
			ClockClass:              protocol.ClockClass(s.ClockClass),
			ClockAccuracy:           protocol.ClockAccuracy(s.ClockAccuracy),
			OffsetScaledLogVariance: s.OffsetScaledLogVariance,
		},
		UTCOffset:  s.CurrentUTCOffset,
		TimeFlags:  f,
		TimeSource: protocol.TimeSource(s.TimeSource),
	}
}

// PMCArgs renders the settings as pmc's SET command line, for logs.
func (s Settings) PMCArgs() string {
	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	return fmt.Sprintf("clockClass %d clockAccuracy 0x%02X offsetScaledLogVariance 0x%04x currentUtcOffset %d "+
		"leap61 %d leap59 %d currentUtcOffsetValid %d ptpTimescale %d timeTraceable %d frequencyTraceable %d timeSource 0x%02X",
		s.ClockClass, s.ClockAccuracy, s.OffsetScaledLogVariance, s.CurrentUTCOffset,
		b(s.Leap61), b(s.Leap59), b(s.CurrentUTCOffsetValid), b(s.PTPTimescale),
		b(s.TimeTraceable), b(s.FrequencyTraceable), s.TimeSource)
}

// Get reads the current settings with a GET (works on the read-only
// socket).
func Get(c *ptp4l.Client) (Settings, error) {
	tlv, err := c.Get(ID, ptp4l.AllPorts)
	if err != nil {
		return Settings{}, err
	}
	t, ok := tlv.(*TLV)
	if !ok {
		return Settings{}, fmt.Errorf("unexpected TLV %T", tlv)
	}
	return FromTLV(t), nil
}

var msgHeadSize = binary.Size(protocol.ManagementMsgHead{})

// SetRequest builds the SET message for s, addressed to the clock, as
// pmc -b 0 does.
func SetRequest(s Settings, domain uint8, seq uint16) *protocol.Management {
	return &protocol.Management{
		ManagementMsgHead: protocol.ManagementMsgHead{
			Header: protocol.Header{
				SdoIDAndMsgType:    protocol.NewSdoIDAndMsgType(protocol.MessageManagement, 0),
				Version:            protocol.Version,
				MessageLength:      uint16(msgHeadSize + 6 + dataSize), //nolint:gosec // constant small size
				DomainNumber:       domain,
				SourcePortIdentity: protocol.PortIdentity{PortNumber: uint16(os.Getpid())}, //nolint:gosec // truncation is fine for an identity
				SequenceID:         seq,
				LogMessageInterval: protocol.MgmtLogMessageInterval,
			},
			TargetPortIdentity: protocol.PortIdentity{ClockIdentity: 0xffffffffffffffff, PortNumber: ptp4l.AllPorts},
			ActionField:        protocol.SET,
		},
		TLV: s.TLV(),
	}
}

// Set sends s as a SET to ptp4l's read-write socket at path (uds_address,
// /var/run/ptp4l by default; the read-only socket refuses SET) and returns
// the settings ptp4l answers with, which are its values after the change.
func Set(path string, domain uint8, s Settings, timeout time.Duration) (Settings, error) {
	conn, err := ptp4l.DialSocket(path, time.Now().Add(timeout))
	if err != nil {
		return Settings{}, err
	}
	defer func() { _ = conn.Close() }()
	const seq = 1
	b, err := SetRequest(s, domain, seq).MarshalBinary()
	if err != nil {
		return Settings{}, err
	}
	if _, err := conn.Write(b); err != nil {
		return Settings{}, err
	}
	buf := make([]byte, 1500)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return Settings{}, err
		}
		tlv, err := ptp4l.MatchResponse(buf[:n], ID, seq)
		if errors.Is(err, ptp4l.ErrSkip) {
			continue
		}
		if err != nil {
			return Settings{}, err
		}
		t, ok := tlv.(*TLV)
		if !ok {
			return Settings{}, fmt.Errorf("unexpected TLV %T", tlv)
		}
		return FromTLV(t), nil
	}
}
