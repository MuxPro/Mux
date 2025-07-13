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

// SessionStatus defines the type of Mux.Pro frame metadata.
type SessionStatus byte

const (
	// Mux.Pro: Main Connection Control - Version Negotiation
	SessionStatusNegotiateVersion SessionStatus = 0x00
	// Mux.Pro: New Sub-connection Request
	SessionStatusNew SessionStatus = 0x01
	// Mux.Pro: Keep Sub-connection / Data Transfer
	SessionStatusKeep SessionStatus = 0x02
	// Mux.Pro: Close Sub-connection
	SessionStatusEnd SessionStatus = 0x03
	// Mux.Pro: Main Connection Keep-alive
	SessionStatusKeepAlive SessionStatus = 0x04
	// Mux.Pro: Sub-connection Flow Control (Credit Update)
	SessionStatusCreditUpdate SessionStatus = 0x05
)

// OptionData indicates the presence of Extra Data.
const OptionData bitmask.Byte = 0x01

// Mux.Pro: OptionError is used in Mux.Cool, Mux.Pro uses ErrorCode field instead.
// For compatibility, we keep this, but prioritize ErrorCode if present and OptionData is set for End frame.
const OptionError bitmask.Byte = 0x02

// TargetNetwork defines the network protocol for the target.
type TargetNetwork byte

const (
	TargetNetworkTCP TargetNetwork = 0x01
	TargetNetworkUDP TargetNetwork = 0x02
)

var addrParser = protocol.NewAddressParser(
	protocol.AddressFamilyByte(byte(protocol.AddressTypeIPv4), net.AddressFamilyIPv4),
	protocol.AddressFamilyByte(byte(protocol.AddressTypeDomain), net.AddressFamilyDomain),
	protocol.AddressFamilyByte(byte(protocol.AddressTypeIPv6), net.AddressFamilyIPv6),
)

// Mux.Pro Error Codes for SessionStatusEnd frame.
const (
	ErrorCodeGracefulShutdown     uint16 = 0x0000 // Graceful Shutdown.
	ErrorCodeRemoteDisconnect    uint16 = 0x0001 // The remote target disconnected.
	ErrorCodeOperationTimeout    uint16 = 0x0002 // An operation on the Sub-connection timed out.
	ErrorCodeProtocolError       uint16 = 0x0003 // A protocol violation occurred on this Sub-connection.
	ErrorCodeResourceExhaustion  uint16 = 0x0004 // The server or client ran out of resources for this Sub-connection.
	ErrorCodeDestinationUnreachable uint16 = 0x0005 // The target address was unreachable.
	ErrorCodeProxyRejected       uint16 = 0x0006 // The proxy server rejected the Sub-connection request.
	// Implementations MAY define additional error codes for specific scenarios.
)

// FrameMetadata contains the control information for a Mux.Pro frame.
type FrameMetadata struct {
	Target        net.Destination
	SessionID     uint16
	Option        bitmask.Byte
	SessionStatus SessionStatus

	// Mux.Pro Extensions:
	Priority  byte   // For SessionStatusNew: MuxPro Extension, scheduling priority.
	ErrorCode uint16 // For SessionStatusEnd: MuxPro Extension, reason for closure.
}

// WriteTo encodes FrameMetadata into the given buffer.
func (f *FrameMetadata) WriteTo(b *buf.Buffer) error {
	lenBytes := b.Extend(2) // Placeholder for Metadata Length L
	len0 := b.Len()        // Current length before writing metadata content

	// ID (2 bytes)
	binary.BigEndian.PutUint16(b.Extend(2), f.SessionID)
	// State S (1 byte)
	common.Must(b.WriteByte(byte(f.SessionStatus)))
	// Opt (1 byte)
	common.Must(b.WriteByte(byte(f.Option)))

	// Write specific fields based on SessionStatus
	switch f.SessionStatus {
	case SessionStatusNegotiateVersion:
		// ID must be 0x0000, State 0x00, Opt 0x01 (Data Present)
		if f.SessionID != 0x0000 || f.Option != OptionData {
			return newError("NegotiateVersion frame must have ID 0x0000 and Opt 0x01").AtError()
		}
		// Version Count N and Version List are in Extra Data, not here.
	case SessionStatusNew:
		// Mux.Pro Extension: Priority P (1 byte)
		common.Must(b.WriteByte(f.Priority))
		// Network Type N (1 byte)
		switch f.Target.Network {
		case net.Network_TCP:
			common.Must(b.WriteByte(byte(TargetNetworkTCP)))
		case net.Network_UDP:
			common.Must(b.WriteByte(byte(TargetNetworkUDP)))
		default:
			return newError("unknown network type: ", f.Target.Network).AtError()
		}

		// Port (2 bytes)
		binary.BigEndian.PutUint16(b.Extend(2), uint16(f.Target.Port))

		// Address Type T (1 byte) and Address A (Variable Length)
		common.Must(addrParser.WriteAddress(b, f.Target.Address))
	case SessionStatusEnd:
		// Mux.Pro Extension: Error Code (2 bytes)
		if f.Option.Has(OptionData) { // In Mux.Pro, ErrorCode is present if Opt(D) is set for End frames
			errorBytes := b.Extend(2)
			binary.BigEndian.PutUint16(errorBytes, f.ErrorCode)
		}
	case SessionStatusKeepAlive:
		// Mux.Pro: Opt MUST be 0x00. No Extra Data.
		if f.Option != 0x00 {
			return newError("Mux.Pro KeepAlive Opt must be 0x00").AtError()
		}
		// No additional metadata fields
	case SessionStatusCreditUpdate:
		// Mux.Pro: Opt MUST be 0x01 (Data Present). Credit Increment is in Extra Data.
		if f.Option != OptionData {
			return newError("Mux.Pro CreditUpdate Opt must be 0x01").AtError()
		}
		// No additional metadata fields, Credit Increment is in Extra Data.
	}

	// Write Metadata Length L
	len1 := b.Len()
	binary.BigEndian.PutUint16(lenBytes, uint16(len1-len0))
	return nil
}

// Unmarshal reads FrameMetadata from the given reader.
func (f *FrameMetadata) Unmarshal(reader io.Reader) error {
	metaLen, err := serial.ReadUint16(reader)
	if err != nil {
		return err
	}
	if metaLen > 512 { // Prevent excessively large metadata
		return newError("invalid metalen ", metaLen).AtError()
	}

	b := buf.New()
	defer b.Release()

	if _, err := b.ReadFullFrom(reader, int32(metaLen)); err != nil {
		return err
	}
	return f.UnmarshalFromBuffer(b)
}

// UnmarshalFromBuffer reads a FrameMetadata from the given buffer.
func (f *FrameMetadata) UnmarshalFromBuffer(b *buf.Buffer) error {
	if b.Len() < 4 { // Minimum 4 bytes: ID (2), State (1), Opt (1)
		return newError("insufficient buffer: ", b.Len()).AtError()
	}

	f.SessionID = binary.BigEndian.Uint16(b.BytesTo(2))
	f.SessionStatus = SessionStatus(b.Byte(2))
	f.Option = bitmask.Byte(b.Byte(3))
	f.Target.Network = net.Network_Unknown // Reset Network type before parsing
	b.Advance(4) // Advance past ID, State, Opt

	switch f.SessionStatus {
	case SessionStatusNegotiateVersion:
		if f.SessionID != 0x0000 || f.Option != OptionData {
			return newError("NegotiateVersion frame must have ID 0x0000 and Opt 0x01").AtError()
		}
		// Version Count N and Version List are in Extra Data, not here.
	case SessionStatusNew:
		// Mux.Pro Extension: Priority P (1 byte)
		if b.Len() < 1 { // Priority P
			return newError("insufficient buffer for new session priority: ", b.Len()).AtError()
		}
		f.Priority = b.Byte(0)
		b.Advance(1)

		// Network Type N (1 byte)
		if b.Len() < 1 { // Network Type N
			return newError("insufficient buffer for new session network type: ", b.Len()).AtError()
		}
		switch TargetNetwork(b.Byte(0)) {
		case TargetNetworkTCP:
			f.Target.Network = net.Network_TCP
		case TargetNetworkUDP:
			f.Target.Network = net.Network_UDP
		default:
			return newError("unknown network type: ", b.Byte(0)).AtError()
		}
		b.Advance(1)

		// Port (2 bytes)
		if b.Len() < 2 { // Port
			return newError("insufficient buffer for new session port: ", b.Len()).AtError()
		}
		f.Target.Port = net.Port(binary.BigEndian.Uint16(b.BytesTo(2)))
		b.Advance(2)

		// Address A (Variable Length)
		addr, err := addrParser.ReadAddress(b)
		if err != nil {
			return newError("failed to parse address").Base(err).AtError()
		}
		f.Target.Address = addr
	case SessionStatusEnd:
		// Mux.Pro Extension: Error Code (2 bytes)
		if f.Option.Has(OptionData) { // In Mux.Pro, ErrorCode is present if Opt(D) is set for End frames
			if b.Len() < 2 { // Error Code
				return newError("insufficient buffer for end session error code: ", b.Len()).AtError()
			}
			f.ErrorCode = binary.BigEndian.Uint16(b.BytesTo(2))
			b.Advance(2)
		} else {
			f.ErrorCode = ErrorCodeGracefulShutdown // Implicit if Opt(D) is not set
		}
	case SessionStatusKeepAlive:
		// Mux.Pro: Opt MUST be 0x00. No Extra Data.
		if f.Option != 0x00 {
			return newError("Mux.Pro KeepAlive Opt must be 0x00").AtError()
		}
		// Check for unexpected extra data if OptionData was mistakenly set
		if f.Option.Has(OptionData) {
			return newError("Mux.Pro KeepAlive must not have Extra Data").AtError()
		}
	case SessionStatusCreditUpdate:
		// Mux.Pro: Opt MUST be 0x01 (Data Present). Credit Increment is in Extra Data.
		if f.Option != OptionData {
			return newError("Mux.Pro CreditUpdate Opt must be 0x01").AtError()
		}
		// No additional metadata fields. Credit Increment is in Extra Data.
	}

	if b.Len() > 0 {
		return newError("remaining bytes in metadata buffer: ", b.Len()).AtError()
	}
	return nil
}

// WriteNegotiateVersionFrame writes a NegotiateVersion frame to the given writer.
// versions should be ordered from highest to lowest preference.
func WriteNegotiateVersionFrame(writer buf.Writer, versions []uint32) error {
	meta := FrameMetadata{
		SessionID:     0x0000,                    // Required for NegotiateVersion
		SessionStatus: SessionStatusNegotiateVersion, // Required
		Option:        OptionData,                // Required: Version List is Extra Data
	}

	metaBuf := buf.New()
	defer metaBuf.Release()
	if err := meta.WriteTo(metaBuf); err != nil {
		return newError("failed to write NegotiateVersion metadata").Base(err)
	}

	// Prepare Extra Data: Version Count N (1 byte) + N * Version (4 bytes)
	extraData := buf.New()
	defer extraData.Release()

	common.Must(extraData.WriteByte(byte(len(versions)))) // Version Count N

	for _, v := range versions {
		vBytes := extraData.Extend(4)
		binary.BigEndian.PutUint32(vBytes, v)
	}

	// Write Extra Data Length L
	if _, err := serial.WriteUint16(metaBuf, uint16(extraData.Len())); err != nil {
		return newError("failed to write Extra Data Length for NegotiateVersion").Base(err)
	}

	mb := buf.MultiBuffer{metaBuf, extraData}
	return writer.WriteMultiBuffer(mb)
}

// ReadNegotiateVersionFrame reads a NegotiateVersion frame and its content.
func ReadNegotiateVersionFrame(reader *buf.BufferedReader) (FrameMetadata, []uint32, error) {
	var meta FrameMetadata
	if err := meta.Unmarshal(reader); err != nil {
		return meta, nil, newError("failed to read NegotiateVersion frame metadata").Base(err)
	}

	if meta.SessionID != 0x0000 || meta.SessionStatus != SessionStatusNegotiateVersion || !meta.Option.Has(OptionData) {
		return meta, nil, newError("invalid NegotiateVersion frame: ID or Status or Option mismatch").AtError()
	}

	extraDataLen, err := serial.ReadUint16(reader)
	if err != nil {
		return meta, nil, newError("failed to read NegotiateVersion extra data length").Base(err)
	}
	if extraDataLen == 0 {
		return meta, nil, newError("NegotiateVersion frame must contain version list").AtError()
	}

	extraDataBuf := buf.New()
	defer extraDataBuf.Release()
	if _, err := extraDataBuf.ReadFullFrom(reader, int32(extraDataLen)); err != nil {
		return meta, nil, newError("failed to read NegotiateVersion extra data").Base(err)
	}

	if extraDataBuf.Len() < 1 { // At least 1 byte for Version Count
		return meta, nil, newError("NegotiateVersion extra data too short for version count").AtError()
	}
	versionCount := extraDataBuf.Byte(0)
	extraDataBuf.Advance(1)

	if extraDataBuf.Len() < int32(versionCount)*4 { // Each version is 4 bytes
		return meta, nil, newError("NegotiateVersion extra data too short for versions").AtError()
	}

	versions := make([]uint32, versionCount)
	for i := 0; i < int(versionCount); i++ {
		versions[i] = binary.BigEndian.Uint32(extraDataBuf.BytesTo(4))
		extraDataBuf.Advance(4)
	}

	return meta, versions, nil
}

// WriteCreditUpdateFrame writes a CreditUpdate frame to the given writer.
func WriteCreditUpdateFrame(writer buf.Writer, sessionID uint16, increment uint32) error {
	meta := FrameMetadata{
		SessionID:     sessionID,
		SessionStatus: SessionStatusCreditUpdate,
		Option:        OptionData, // Credit Increment is Extra Data
	}

	metaBuf := buf.New()
	defer metaBuf.Release()
	if err := meta.WriteTo(metaBuf); err != nil {
		return newError("failed to write CreditUpdate metadata").Base(err)
	}

	// Prepare Extra Data: Credit Increment (4 bytes, fixed for simplicity)
	extraData := buf.New()
	defer extraData.Release()
	incBytes := extraData.Extend(4)
	binary.BigEndian.PutUint32(incBytes, increment)

	// Write Extra Data Length L
	if _, err := serial.WriteUint16(metaBuf, uint16(extraData.Len())); err != nil {
		return newError("failed to write Extra Data Length for CreditUpdate").Base(err)
	}

	mb := buf.MultiBuffer{metaBuf, extraData}
	return writer.WriteMultiBuffer(mb)
}

// ReadCreditUpdateFrame reads a CreditUpdate frame and its content.
func ReadCreditUpdateFrame(reader *buf.BufferedReader) (FrameMetadata, uint32, error) {
	var meta FrameMetadata
	if err := meta.Unmarshal(reader); err != nil {
		return meta, 0, newError("failed to read CreditUpdate frame metadata").Base(err)
	}

	if meta.SessionStatus != SessionStatusCreditUpdate || !meta.Option.Has(OptionData) {
		return meta, 0, newError("invalid CreditUpdate frame: Status or Option mismatch").AtError()
	}

	extraDataLen, err := serial.ReadUint16(reader)
	if err != nil {
		return meta, 0, newError("failed to read CreditUpdate extra data length").Base(err)
	}
	if extraDataLen == 0 {
		return meta, 0, newError("CreditUpdate frame must contain credit increment").AtError()
	}
	if extraDataLen != 4 { // Assuming fixed 4-byte uint32 for simplicity
		return meta, 0, newError("CreditUpdate extra data length mismatch, expected 4 bytes").AtError()
	}

	extraDataBuf := buf.New()
	defer extraDataBuf.Release()
	if _, err := extraDataBuf.ReadFullFrom(reader, int32(extraDataLen)); err != nil {
		return meta, 0, newError("failed to read CreditUpdate extra data").Base(err)
	}

	increment := binary.BigEndian.Uint32(extraDataBuf.Bytes())

	return meta, increment, nil
}
