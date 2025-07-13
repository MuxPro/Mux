package mux

import (
	"time"
	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/common/buf"
	"github.com/v2fly/v2ray-core/v5/common/errors"
	"github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/common/protocol"
	"github.com/v2fly/v2ray-core/v5/common/serial"
)

// Writer is responsible for packaging application layer data into Mux frames.
type Writer struct {
	dest             net.Destination
	writer           buf.Writer
	id               uint16
	followup         bool
	transferType     protocol.TransferType
	priority         byte
	currentErrorCode uint16
	session          *Session // Mux.Pro: Reference to the associated Session for flow control
	globalID         GlobalID // Mux.Pro: GlobalID for UDP FullCone NAT
}

// NewWriter creates a new Writer for sending client-initiated Mux frames.
// id: Session ID.
// dest: Target destination.
// writer: Underlying buffer writer.
// transferType: Protocol transfer type (Stream or Packet).
// priority: Mux.Pro field, used to set the priority of a new connection.
// s: Mux.Pro parameter, points to the Session associated with this Writer for flow control.
// globalID: Mux.Pro parameter, GlobalID for UDP FullCone NAT.
func NewWriter(id uint16, dest net.Destination, writer buf.Writer, transferType protocol.TransferType, priority byte, s *Session, globalID GlobalID) *Writer {
	return &Writer{
		id:               id,
		dest:             dest,
		writer:           writer,
		followup:         false,
		transferType:     transferType,
		priority:         priority,
		currentErrorCode: ErrorCodeGracefulShutdown, // Mux.Pro: Default to graceful shutdown
		session:          s,                         // Mux.Pro: Associated Session
		globalID:         globalID,                  // Mux.Pro: GlobalID
	}
}

// NewResponseWriter creates a new Writer for sending server response Mux frames.
// id: Session ID.
// writer: Underlying buffer writer.
// transferType: Protocol transfer type (Stream or Packet).
// s: Mux.Pro parameter, points to the Session associated with this Writer for flow control.
// globalID: Mux.Pro parameter, GlobalID for UDP FullCone NAT.
func NewResponseWriter(id uint16, writer buf.Writer, transferType protocol.TransferType, s *Session, globalID GlobalID) *Writer {
	return &Writer{
		id:               id,
		writer:           writer,
		followup:         true,
		transferType:     transferType,
		priority:         0x00,                        // Mux.Pro: Response Writer default priority is 0
		currentErrorCode: ErrorCodeGracefulShutdown, // Mux.Pro: Default to graceful shutdown
		session:          s,                         // Mux.Pro: Associated Session
		globalID:         globalID,                  // Mux.Pro: GlobalID from client's New frame
	}
}

// SetErrorCode sets the error code to be used by the Writer upon closing.
func (w *Writer) SetErrorCode(code uint16) {
	w.currentErrorCode = code
}

// getNextFrameMeta generates the metadata for the next Mux frame.
func (w *Writer) getNextFrameMeta() FrameMetadata {
	meta := FrameMetadata{
		SessionID: w.id,
		Target:    w.dest,
		GlobalID:  w.globalID, // Mux.Pro: Include GlobalID in metadata for New frames
	}

	if w.followup {
		meta.SessionStatus = SessionStatusKeep
	} else {
		w.followup = true
		meta.SessionStatus = SessionStatusNew
		// Mux.Pro: For SessionStatusNew frames, set priority
		meta.Priority = w.priority
	}

	return meta
}

// writeMetaOnly sends a metadata-only frame (without extra data).
func (w *Writer) writeMetaOnly() error {
	meta := w.getNextFrameMeta()
	b := buf.New()
	if err := meta.WriteTo(b); err != nil {
		return err
	}
	return w.writer.WriteMultiBuffer(buf.MultiBuffer{b})
}

// writeMetaWithFrame writes metadata and extra data.
// writer: The underlying buf.Writer.
// meta: The FrameMetadata to be written.
// data: The actual data payload (Extra Data).
func writeMetaWithFrame(writer buf.Writer, meta FrameMetadata, data buf.MultiBuffer) error {
	frame := buf.New()
	if err := meta.WriteTo(frame); err != nil {
		return err
	}
	// Mux.Pro: Extra Data itself has a length field
	if _, err := serial.WriteUint16(frame, uint16(data.Len())); err != nil {
		return err
	}

	if len(data)+1 > 64*1024*1024 { // Check if total length is too large
		return errors.New("value too large")
	}
	sliceSize := len(data) + 1
	mb2 := make(buf.MultiBuffer, 0, sliceSize)
	mb2 = append(mb2, frame)
	mb2 = append(mb2, data...)
	return writer.WriteMultiBuffer(mb2)
}

// writeDataInternal is an internal function that actually writes data frames, without flow control logic.
// It constructs the full Extra Data payload including any XUDP specific prefixes.
// packetDest: For UDP packets, this is the associated net.Destination (e.g., source for responses, target for requests).
func (w *Writer) writeDataInternal(mb buf.MultiBuffer, packetDest *net.Destination) error {
	meta := w.getNextFrameMeta()
	meta.Option.Set(OptionData) // Set OptionData bit, indicating extra data presence

	var fullPayload buf.MultiBuffer // This will hold the XUDP prefix (if any) + actual data

	// Mux.Pro: Handle XUDP extension for Keep frames (for UDP packet transfer type)
	// This applies to both client sending UDP data and server sending UDP responses.
	if meta.SessionStatus == SessionStatusKeep && w.transferType == protocol.TransferTypePacket {
		// Check if packetDest is provided (meaning it's a UDP packet with specific address info)
		if packetDest != nil {
			meta.Option.Set(OptionUDPData) // Set the new OptionUDPData bit in metadata

			addrPrefix := buf.New()
			// XUDP format for Extra Data prefix: 1 byte network type (0x02 for UDP), then address/port
			common.Must(addrPrefix.WriteByte(byte(TargetNetworkUDP)))
			if err := addrParser.WriteAddressPort(addrPrefix, packetDest.Address, packetDest.Port); err != nil {
				addrPrefix.Release()
				return newError("failed to write UDP address/port prefix").Base(err)
			}
			// If GlobalID is present (e.g., server sending response with client's GlobalID), append it here
			if w.globalID != (GlobalID{}) {
				common.Must2(addrPrefix.Write(w.globalID[:]))
			}
			fullPayload = append(fullPayload, addrPrefix)
		}
	}
	
	fullPayload = append(fullPayload, mb...) // Append the actual data buffers

	return writeMetaWithFrame(w.writer, meta, fullPayload)
}

// WriteMultiBuffer implements buf.Writer interface, writing MultiBuffer to Mux frames.
// Includes Mux.Pro's credit-based flow control logic.
func (w *Writer) WriteMultiBuffer(mb buf.MultiBuffer) error {
	defer buf.ReleaseMulti(mb)

	if mb.IsEmpty() {
		return w.writeMetaOnly()
	}

	for !mb.IsEmpty() {
		var currentChunk buf.MultiBuffer // Used to process the current chunk being sent
		var packetDest *net.Destination // For UDP packets, this will hold the associated address/port

		if w.transferType == protocol.TransferTypeStream {
			// Stream mode splits by 8KB, or all remaining data
			if mb.Len() > 8*1024 {
				currentChunk, mb = buf.SplitSize(mb, 8*1024)
			} else {
				currentChunk = mb
				mb = nil // All data processed
			}
			packetDest = nil // No specific UDP destination for stream data
		} else {
			// Packet mode takes the whole MultiBuffer as one packet.
			// The UDP metadata (source/destination for the packet) needs to be available.
			// Since buf.MultiBuffer does not have a .UDP field in the provided definition,
			// we temporarily use w.dest. This needs to be refined for full FullCone NAT.
			currentChunk = mb 
			mb = nil // All data processed for this iteration

			// IMPORTANT: This is a placeholder for the actual UDP packet's source/destination.
			// In a full XUDP implementation, this `packetDest` would come from the
			// `buf.Buffer` itself (if it supported a .UDP field) or be passed explicitly
			// by the upstream `buf.Reader` (e.g., a custom PacketReader).
			// For client-side outbound UDP, w.dest is the target.
			// For server-side inbound UDP responses, this needs to be the *source* of the response.
			packetDest = &w.dest // Temporary: Use session's target as packet destination
		}

		chunkLen := uint32(currentChunk.Len())
		if chunkLen == 0 { // Empty chunk, skip directly
			continue
		}

		for chunkLen > 0 { // Loop until the current chunk is fully sent
			// Mux.Pro: Before attempting to consume credit or wait, check if SessionManager is closing
			if w.session.parent.Closed() {
				return newError("session manager closed while waiting for credit for session ", w.id).AtWarning()
			}

			consumed, ok := w.session.ConsumeCredit(chunkLen)
			if ok {
				// Successfully consumed credit, send the corresponding part of the data
				partToSend, remainingInChunk := buf.SplitSize(currentChunk, int32(consumed))
				if err := w.writeDataInternal(partToSend, packetDest); err != nil { // Pass packetDest
					return err
				}
				currentChunk = remainingInChunk // Update currentChunk to the remaining unsent part
				chunkLen -= consumed
			} else {
				// No credit available, wait for credit update signal or timeout
				select {
				case <-w.session.creditUpdate:
					// Received credit update signal, continue trying to consume credit
					continue
				case <-time.After(time.Second * 30): // Set timeout to prevent indefinite blocking
					return newError("writer for session ", w.id, " blocked due to no credit, timed out").AtWarning()
				}
			}
		}
	}

	return nil
}

// Close implements common.Closable interface, closes the Writer and sends an End frame.
func (w *Writer) Close() error {
	meta := FrameMetadata{
		SessionID:     w.id,
		SessionStatus: SessionStatusEnd,
	}

	// Mux.Pro: If the error code is not graceful shutdown, set OptionData bit and include error code.
	if w.currentErrorCode != ErrorCodeGracefulShutdown {
		meta.Option.Set(OptionData) // D(0x01) bit in End frame indicates ErrorCode presence
		meta.ErrorCode = w.currentErrorCode
	}

	frame := buf.New()
	common.Must(meta.WriteTo(frame))

	// Attempt to write End frame, do not handle error as connection might be broken.
	w.writer.WriteMultiBuffer(buf.MultiBuffer{frame})
	return nil
}
