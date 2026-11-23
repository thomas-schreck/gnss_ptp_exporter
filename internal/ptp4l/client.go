// Package ptp4l queries linuxptp's ptp4l over its management Unix socket.
//
// The client only ever sends GET requests. Point it at ptp4l's read-only
// socket (uds_ro_address, /var/run/ptp4lro by default), which rejects
// anything but GET, so the exporter cannot change ptp4l even by mistake.
package ptp4l

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/facebook/time/ptp/protocol"
)

// PortDataSetTLV is the PORT_DATA_SET management TLV (IEEE 1588-2019,
// 15.5.3.7.1). facebook/time knows its ID but has no decoder for it.
type PortDataSetTLV struct {
	protocol.ManagementTLVHead

	PortIdentity            protocol.PortIdentity
	PortState               protocol.PortState
	LogMinDelayReqInterval  protocol.LogInterval
	PeerMeanPathDelay       protocol.TimeInterval
	LogAnnounceInterval     protocol.LogInterval
	AnnounceReceiptTimeout  uint8
	LogSyncInterval         protocol.LogInterval
	DelayMechanism          uint8
	LogMinPdelayReqInterval protocol.LogInterval
	VersionNumber           uint8
}

// DecodePortDataSet decodes a PORT_DATA_SET TLV (starting at the TLV type).
func DecodePortDataSet(data []byte) (protocol.ManagementTLV, error) {
	tlv := &PortDataSetTLV{}
	if err := binary.Read(bytes.NewReader(data), binary.BigEndian, tlv); err != nil {
		return nil, err
	}
	return tlv, nil
}

func init() {
	protocol.RegisterMgmtTLVDecoder(protocol.IDPortDataSet, DecodePortDataSet)
}

// getTLV is a management TLV without data field, as pmc sends for GET.
type getTLV struct {
	protocol.ManagementTLVHead
}

func (t *getTLV) MarshalBinary() ([]byte, error) {
	var b bytes.Buffer
	err := binary.Write(&b, binary.BigEndian, t.ManagementTLVHead)
	return b.Bytes(), err
}

// AllPorts targets every port of the clock.
const AllPorts = 0xffff

var (
	msgHeadSize = binary.Size(protocol.ManagementMsgHead{})
	tlvGetSize  = binary.Size(protocol.ManagementTLVHead{})
)

// GetRequest builds a GET management message for id, addressed to port
// (AllPorts for the clock). It is the only kind of message this package
// builds.
func GetRequest(id protocol.ManagementID, port uint16, domain uint8, seq uint16) *protocol.Management {
	return &protocol.Management{
		ManagementMsgHead: protocol.ManagementMsgHead{
			Header: protocol.Header{
				SdoIDAndMsgType:    protocol.NewSdoIDAndMsgType(protocol.MessageManagement, 0),
				Version:            protocol.Version,
				MessageLength:      uint16(msgHeadSize + tlvGetSize), //nolint:gosec // constant small size
				DomainNumber:       domain,
				SourcePortIdentity: protocol.PortIdentity{PortNumber: uint16(os.Getpid())}, //nolint:gosec // truncation is fine for an identity
				SequenceID:         seq,
				LogMessageInterval: protocol.MgmtLogMessageInterval,
			},
			TargetPortIdentity: protocol.PortIdentity{ClockIdentity: 0xffffffffffffffff, PortNumber: port},
			ActionField:        protocol.GET,
		},
		TLV: &getTLV{ManagementTLVHead: protocol.ManagementTLVHead{
			TLVHead:      protocol.TLVHead{TLVType: protocol.TLVManagement, LengthField: 2},
			ManagementID: id,
		}},
	}
}

// Client is a short-lived connection to ptp4l's management socket.
type Client struct {
	conn   *net.UnixConn
	domain uint8
	seq    uint16
}

var clientSeq atomic.Uint64

// Dial connects to the ptp4l socket at path. The client binds an abstract
// Unix socket so that ptp4l can answer even when it runs with PrivateTmp
// or the exporter has no writable directory.
func Dial(path string, domain uint8, deadline time.Time) (*Client, error) {
	laddr := &net.UnixAddr{Net: "unixgram",
		Name: fmt.Sprintf("@gnss_ptp_exporter.%d.%d", os.Getpid(), clientSeq.Add(1))}
	conn, err := net.DialUnix("unixgram", laddr, &net.UnixAddr{Net: "unixgram", Name: path})
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &Client{conn: conn, domain: domain}, nil
}

// Close closes the socket.
func (c *Client) Close() error { return c.conn.Close() }

// ErrorStatus is a MANAGEMENT_ERROR_STATUS answer from ptp4l.
type ErrorStatus struct {
	ID   protocol.ManagementID
	Code protocol.ManagementErrorID
}

func (e *ErrorStatus) Error() string {
	return fmt.Sprintf("ptp4l answered management ID 0x%04x with %s", uint16(e.ID), e.Code.String())
}

// Get sends a GET for id to port and returns the matching response TLV.
// Responses with another sequence number or ID are skipped.
func (c *Client) Get(id protocol.ManagementID, port uint16) (protocol.ManagementTLV, error) {
	c.seq++
	req := GetRequest(id, port, c.domain, c.seq)
	b, err := req.MarshalBinary()
	if err != nil {
		return nil, err
	}
	if _, err := c.conn.Write(b); err != nil {
		return nil, err
	}
	buf := make([]byte, 1500)
	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			return nil, err
		}
		tlv, err := matchResponse(buf[:n], id, c.seq)
		if errors.Is(err, errSkip) {
			continue
		}
		return tlv, err
	}
}

var errSkip = errors.New("not the response we wait for")

// matchResponse decodes a datagram and checks that it answers request
// (id, seq).
func matchResponse(b []byte, id protocol.ManagementID, seq uint16) (protocol.ManagementTLV, error) {
	pkt, err := protocol.DecodePacket(b)
	if err != nil {
		return nil, fmt.Errorf("decoding ptp4l response: %w", err)
	}
	switch p := pkt.(type) {
	case *protocol.Management:
		if p.SequenceID != seq || p.TLV == nil || p.TLV.MgmtID() != id {
			return nil, errSkip
		}
		if p.Action() != protocol.RESPONSE {
			return nil, errSkip
		}
		return p.TLV, nil
	case *protocol.ManagementMsgErrorStatus:
		if p.SequenceID != seq {
			return nil, errSkip
		}
		return nil, &ErrorStatus{ID: id, Code: p.ManagementErrorID}
	}
	return nil, errSkip
}
