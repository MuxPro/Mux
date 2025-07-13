package mux

import (
	"encoding/binary"
	"io"

	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/common/bitmask"
	"github.com/v2fly/v2ray-core/v5/common/buf"
	"github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/common/protocol"
	"github.com/v2fly/v2ray-core/v5/common/serial"
)

// Version represents the current Mux.Pro protocol version.
// Major (2 bytes) | Minor (2 bytes)
const (
	Version uint32 = 0x00000001 // Mux.Pro protocol version 0.1
)

// SessionStatus defines the type and purpose of a frame.
type SessionStatus byte

const (
	SessionStatusNegotiateVersion SessionStatus = 0x00 // Mux.Pro: Version negotiation
	SessionStatusNew              SessionStatus = 0x01 // New sub-connection
	SessionStatusKeep             SessionStatus = 0x02 // Data transfer
	SessionStatusEnd              SessionStatus = 0x03 // Close sub-connection
	SessionStatusKeepAlive        SessionStatus = 0x04 // Keep main connection alive
	SessionStatusCreditUpdate     SessionStatus = 0x05 // Mux.Pro: Credit update (flow control)
)

// Option defines option bits within the metadata.
const (
	// OptionData (0x01): Data Present.
	// For data frames (New, Keep), indicates Extra Data presence.
	// For End frames, indicates ErrorCode presence.
	// For NegotiateVersion and CreditUpdate frames, indicates Extra Data presence.
	OptionData bitmask.Byte = 0x01

	// OptionError (0x02): Used in Mux.Cool to indicate error, replaced by ErrorCode in Mux.Pro.
	OptionError bitmask.Byte = 0x02 // Deprecated in Mux.Pro
)

// ErrorCode defines error codes in Mux.Pro End frames.
const (
	ErrorCodeGracefulShutdown   uint16 = 0x0000 // Normal closure
	ErrorCodeRemoteDisconnect   uint16 = 0x0001 // Remote target disconnected
	ErrorCodeOperationTimeout   uint16 = 0x0002 // Operation timed out
	ErrorCodeProtocolError      uint16 = 0x0003 // Protocol violation
	ErrorCodeResourceExhaustion uint16 = 0x0004 // Resource exhaustion
	ErrorCodeDestUnreachable    uint16 = 0x0005 // Destination unreachable
	ErrorCodeProxyRejected      uint16 = 0x0006 // Proxy rejected request
)

// TargetNetwork specifies the network protocol for the target.
type TargetNetwork byte

const (
	TargetNetworkTCP TargetNetwork = 0x01
	TargetNetworkUDP TargetNetwork = 0x02
)

// GlobalID is an 8-byte identifier for a UDP session, used for FullCone NAT.
type GlobalID [8]byte

var addrParser = protocol.NewAddressParser(
	protocol.AddressFamilyByte(byte(protocol.AddressTypeIPv4), net.AddressFamilyIPv4),
	protocol.AddressFamilyByte(byte(protocol.AddressTypeDomain), net.AddressFamilyDomain),
	protocol.AddressFamilyByte(byte(protocol.AddressTypeIPv6), net.AddressFamilyIPv6),
	protocol.PortThenAddress(),
)

/*
Mux.Pro Frame Format
+-------------------+-----------------+----------------+
| 2 bytes           | L bytes         | X bytes        |
+-------------------+-----------------+----------------+
| Metadata Length L | Metadata        | Extra Data     |
+-------------------+-----------------+----------------+

Metadata Format
+-----------+---------------+-------------------+---------------------+----------------+
| 2 bytes   | 1 byte        | 1 byte            | Variable Length     | 8 bytes (XUDP) |
+-----------+---------------+-------------------+---------------------+----------------+
| ID        | State S       | Option Opt        | Options/Fields...   | GlobalID (UDP) |
+-----------+---------------+-------------------+---------------------+----------------+
*/

// FrameMetadata represents the metadata part of a Mux frame.
type FrameMetadata struct {
	Target        net.Destination
	SessionID     uint16
	Option        bitmask.Byte
	SessionStatus SessionStatus

	// Mux.Pro specific fields
	Priority  byte   // Used in New frames
	ErrorCode uint16 // Used in End frames
	GlobalID  GlobalID // Mux.Pro: Used in New frames for UDP FullCone NAT
}

// WriteTo serializes the metadata into the buffer.
func (f *FrameMetadata) WriteTo(b *buf.Buffer) error {
	lenBytes := b.Extend(2)
	len0 := b.Len()

	sessionBytes := b.Extend(2)
	binary.BigEndian.PutUint16(sessionBytes, f.SessionID)

	common.Must(b.WriteByte(byte(f.SessionStatus)))
	common.Must(b.WriteByte(byte(f.Option)))

	switch f.SessionStatus {
	case SessionStatusNew:
		switch f.Target.Network {
		case net.Network_TCP:
			common.Must(b.WriteByte(byte(TargetNetworkTCP)))
		case net.Network_UDP:
			common.Must(b.WriteByte(byte(TargetNetworkUDP)))
		}
		// Mux.Pro: Write priority field
		common.Must(b.WriteByte(f.Priority))

		if err := addrParser.WriteAddressPort(b, f.Target.Address, f.Target.Port); err != nil {
			return err
		}
		// Mux.Pro: If UDP, write GlobalID
		if f.Target.Network == net.Network_UDP && f.GlobalID != [8]byte{} {
			Must2(b.Write(f.GlobalID[:]))
		}
	case SessionStatusEnd:
		// Mux.Pro: For End frames, OptionData (D) indicates ErrorCode presence.
		if f.Option.Has(OptionData) {
			if err := WriteUint16(b, f.ErrorCode); err != nil {
				return err
			}
		}
	}

	len1 := b.Len()
	binary.BigEndian.PutUint16(lenBytes, uint16(len1-len0))
	return nil
}

// Unmarshal reads and parses metadata from the reader.
func (f *FrameMetadata) Unmarshal(reader io.Reader) error {
	metaLen, err := serial.ReadUint16(reader)
	if err != nil {
		return err
	}
	if metaLen > 512 { // Limit metadata length to prevent abuse
		return newError("invalid metalen ", metaLen).AtError()
	}

	b := buf.New()
	defer b.Release()

	if _, err := b.ReadFullFrom(reader, int32(metaLen)); err != nil {
		return err
	}
	return f.UnmarshalFromBuffer(b)
}

// UnmarshalFromBuffer parses metadata from the given buffer.
func (f *FrameMetadata) UnmarshalFromBuffer(b *buf.Buffer) error {
	if b.Len() < 4 {
		return newError("insufficient buffer for metadata header: ", b.Len())
	}

	f.SessionID = binary.BigEndian.Uint16(b.Bytes())
	f.SessionStatus = SessionStatus(b.Byte(2))
	f.Option = bitmask.Byte(b.Byte(3))
	b.Advance(4) // Consume the read header

	// Set default values
	f.Target.Network = net.Network_Unknown
	f.ErrorCode = ErrorCodeGracefulShutdown
	f.GlobalID = [8]byte{} // Initialize GlobalID to zero

	switch f.SessionStatus {
	case SessionStatusNew:
		// At least 1 byte for network type and 1 byte for priority
		if b.Len() < 2 {
			return newError("insufficient buffer for New frame fields: ", b.Len())
		}
		network := TargetNetwork(b.Byte(0))
		// Mux.Pro: Read priority field
		f.Priority = b.Byte(1)
		b.Advance(2)

		addr, port, err := addrParser.ReadAddressPort(nil, b)
		if err != nil {
			return newError("failed to parse address and port for New frame").Base(err)
		}

		switch network {
		case TargetNetworkTCP:
			f.Target = net.TCPDestination(addr, port)
		case TargetNetworkUDP:
			f.Target = net.UDPDestination(addr, port)
		default:
			return newError("unknown network type: ", network)
		}
		// Mux.Pro: If UDP, read GlobalID
		if f.Target.Network == net.Network_UDP && b.Len() >= 8 {
			copy(f.GlobalID[:], b.Bytes()[:8])
			b.Advance(8)
		}
	case SessionStatusEnd:
		// Mux.Pro: For End frames, OptionData (D) indicates ErrorCode presence.
		if f.Option.Has(OptionData) {
			if b.Len() < 2 {
				return newError("insufficient buffer for End frame error code: ", b.Len())
			}
			f.ErrorCode = binary.BigEndian.Uint16(b.Bytes())
			b.Advance(2)
		}
	}

	return nil
}
