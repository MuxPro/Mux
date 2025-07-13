package mux

import (
	"encoding/binary"
	"io"

	"github.com/v2fly/v2ray-core/v5/common/buf"
	"github.com/v2fly/v2ray-core/v5/common/crypto"
	"github.com/v2fly/v2ray-core/v5/common/net" // 引入 net 包
	"github.com/v2fly/v2ray-core/v5/common/serial"
)

// PacketReader is an io.Reader that reads whole chunk of Mux frames every time.
type PacketReader struct {
	reader *buf.BufferedReader // Use BufferedReader for efficient reading
	eof    bool
}

// NewPacketReader creates a new PacketReader.
func NewPacketReader(reader io.Reader) *PacketReader {
	return &PacketReader{
		reader: &buf.BufferedReader{Reader: reader}, // Wrap with BufferedReader
		eof:    false,
	}
}

// ReadMultiBuffer implements buf.Reader.
func (r *PacketReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	if r.eof {
		return nil, io.EOF
	}

	var meta FrameMetadata
	// Unmarshal reads metadata length, then metadata itself.
	err := meta.Unmarshal(r.reader)
	if err != nil {
		if errors.Cause(err) == io.EOF {
			r.eof = true // Mark EOF if underlying reader returns EOF
		}
		return nil, err
	}

	// Read Extra Data length (if OptionData is set)
	var extraDataLen uint16
	if meta.Option.Has(OptionData) {
		extraDataLen, err = serial.ReadUint16(r.reader)
		if err != nil {
			return nil, newError("failed to read Extra Data length").Base(err)
		}
	}

	// Read actual payload size (this is the size field after metadata and extra data length)
	payloadSize, err := serial.ReadUint16(r.reader)
	if err != nil {
		return nil, newError("failed to read payload size").Base(err)
	}

	if payloadSize > buf.Size { // Check against max buffer size
		return nil, newError("payload size too large: ", payloadSize)
	}

	// Create a new buffer to hold the payload
	b := buf.New()
	if _, err := b.ReadFullFrom(r.reader, int32(payloadSize)); err != nil {
		b.Release()
		return nil, newError("failed to read payload").Base(err)
	}

	// Handle Extra Data based on SessionStatus and Options
	if meta.Option.Has(OptionData) {
		extraDataBuffer := buf.New()
		defer extraDataBuffer.Release()

		if _, err := extraDataBuffer.ReadFullFrom(r.reader, int32(extraDataLen)); err != nil {
			b.Release()
			return nil, newError("failed to read Extra Data").Base(err)
		}

		switch meta.SessionStatus {
		case SessionStatusKeep:
			// 如果 meta.Option.Has(OptionUDPData)，从帧的 Extra Data 中解析出 UDP 源地址和端口
			if meta.Option.Has(OptionUDPData) {
				if extraDataBuffer.Len() < 1 {
					return nil, newError("insufficient Extra Data for UDP network type").AtError()
				}
				networkByte := extraDataBuffer.Byte(0)
				extraDataBuffer.Advance(1) // Consume network type byte

				if TargetNetwork(networkByte) != TargetNetworkUDP {
					return nil, newError("unexpected network type in UDP Keep frame Extra Data: ", networkByte).AtError()
				}

				addr, port, err := addrParser.ReadAddressPort(nil, extraDataBuffer)
				if err != nil {
					b.Release()
					return nil, newError("failed to parse UDP address/port from Extra Data").Base(err)
				}
				b.UDPInfo = &net.Destination{Network: net.Network_UDP, Address: addr, Port: port}

				// 同时，如果帧中包含 GlobalID，也将其解析出来。
				if extraDataBuffer.Len() >= 8 { // GlobalID is 8 bytes
					copy(b.GlobalID[:], extraDataBuffer.Bytes()[:8])
					extraDataBuffer.Advance(8)
				}
			}
		case SessionStatusNew:
			// For SessionStatusNew, GlobalID is part of FrameMetadata, not Extra Data.
			// This is already handled by meta.Unmarshal.
			if meta.Target.Network == net.Network_UDP {
				b.GlobalID = meta.GlobalID // Assign GlobalID from metadata to buffer
			}
		case SessionStatusCreditUpdate:
			// CreditUpdate Extra Data is handled in client/server worker. No need to attach to buffer.
		case SessionStatusNegotiateVersion:
			// Negotiation Extra Data is handled in client/server worker. No need to attach to buffer.
		}
	}

	// r.eof = true // This should only be true if the underlying reader is truly exhausted.
	// For continuous Mux streams, this should not be here.
	return buf.MultiBuffer{b}, nil
}

// NewStreamReader creates a new StreamReader.
func NewStreamReader(reader *buf.BufferedReader) buf.Reader {
	return crypto.NewChunkStreamReaderWithChunkCount(crypto.PlainChunkSizeParser{}, reader, 1)
}
