package mux

import (
	"encoding/binary"
	"io"

	"github.com/v2fly/v2ray-core/v5/common/buf"
	"github.com/v2fly/v2ray-core/v5/common/crypto"
	"github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/common/protocol"
	"github.com/v2fly/v2ray-core/v5/common/serial"
)

// PacketReader is an io.Reader that reads whole chunk of Mux frames every time.
type PacketReader struct {
	reader io.Reader
	eof    bool
}

// NewPacketReader creates a new PacketReader.
func NewPacketReader(reader io.Reader) *PacketReader {
	return &PacketReader{
		reader: reader,
		eof:    false,
	}
}

// ReadMultiBuffer implements buf.Reader.
// This method reads a single Mux packet (length + data). It does not parse FrameMetadata.
// FrameMetadata parsing and UDP/GlobalID extraction from Extra Data will happen in higher layers.
func (r *PacketReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	if r.eof {
		return nil, io.EOF
	}

	size, err := serial.ReadUint16(r.reader)
	if err != nil {
		return nil, err
	}

	if size > buf.Size {
		return nil, newError("packet size too large: ", size)
	}

	b := buf.New()
	if _, err := b.ReadFullFrom(r.reader, int32(size)); err != nil {
		b.Release()
		return nil, err
	}
	r.eof = true
	return buf.MultiBuffer{b}, nil
}

// NewStreamReader creates a new StreamReader.
// This reader is used to read the *payload data* part of a Mux frame.
func NewStreamReader(reader *buf.BufferedReader) buf.Reader {
	return crypto.NewChunkStreamReaderWithChunkCount(crypto.PlainChunkSizeParser{}, reader, 1)
}

// readUDPMetaData reads UDP source address, port, and GlobalID from the Extra Data section of a Mux frame.
// It assumes the 'reader' is positioned at the beginning of the Extra Data payload
// (after the Extra Data length field has been read).
// This function is called by frame handlers (e.g., client.handleStatusKeep) after parsing FrameMetadata
// and detecting the OptionUDPData bit.
func readUDPMetaData(reader *buf.BufferedReader) (net.Destination, GlobalID, error) {
	// First, read the length of the Extra Data payload itself.
	// This length field is part of the Mux frame structure, specifically for OptionData.
	dataLen, err := serial.ReadUint16(reader)
	if err != nil {
		return net.Destination{}, GlobalID{}, newError("failed to read UDP metadata length").Base(err)
	}

	payload := buf.New()
	defer payload.Release()

	// Read the actual Extra Data bytes into a buffer.
	if _, err := payload.ReadFullFrom(reader, int32(dataLen)); err != nil {
		return net.Destination{}, GlobalID{}, newError("failed to read UDP metadata payload").Base(err)
	}

	// Expected format for UDP metadata within Extra Data:
	// 1 byte: Address type (similar to SOCKS5, e.g., IPv4, IPv6, Domain)
	// Variable: Address (4 bytes for IPv4, 16 for IPv6, 1+len for Domain)
	// 2 bytes: Port (uint16)
	// 8 bytes: GlobalID (if present, always 8 bytes)

	if payload.Len() < 3 { // Minimum for address type (1 byte) + port (2 bytes)
		return net.Destination{}, GlobalID{}, newError("insufficient data for UDP metadata: payload too short").AtWarning()
	}

	addrType := payload.Byte(0)
	payload.Advance(1) // Consume address type byte

	var addr net.Address
	switch addrType {
	case protocol.AddressTypeIPv4:
		if payload.Len() < 4 {
			return net.Destination{}, GlobalID{}, newError("insufficient data for IPv4 address").AtWarning()
		}
		addr = net.IPAddress(payload.BytesTo(4))
		payload.Advance(4)
	case protocol.AddressTypeIPv6:
		if payload.Len() < 16 {
			return net.Destination{}, GlobalID{}, newError("insufficient data for IPv6 address").AtWarning()
		}
		addr = net.IPAddress(payload.BytesTo(16))
		payload.Advance(16)
	case protocol.AddressTypeDomain:
		if payload.Len() < 1 {
			return net.Destination{}, GlobalID{}, newError("insufficient data for domain length").AtWarning()
		}
		domainLen := payload.Byte(0)
		payload.Advance(1) // Consume domain length byte
		if payload.Len() < int(domainLen) {
			return net.Destination{}, GlobalID{}, newError("insufficient data for domain").AtWarning()
		}
		addr = net.DomainAddress(string(payload.BytesTo(int(domainLen))))
		payload.Advance(int(domainLen))
	default:
		return net.Destination{}, GlobalID{}, newError("unknown address type in UDP metadata: ", addrType).AtWarning()
	}

	if payload.Len() < 2 {
		return net.Destination{}, GlobalID{}, newError("insufficient data for port in UDP metadata").AtWarning()
	}
	port := net.Port(binary.BigEndian.Uint16(payload.BytesTo(2)))
	payload.Advance(2) // Consume port bytes

	var globalID GlobalID
	if payload.Len() >= 8 { // Check if GlobalID is present (it should be for Mux.Pro UDP)
		copy(globalID[:], payload.BytesTo(8))
		payload.Advance(8) // Consume GlobalID bytes
	} else {
		newError("GlobalID missing in UDP metadata").AtWarning().WriteToLog()
	}

	return net.UDPDestination(addr, port), globalID, nil
}
