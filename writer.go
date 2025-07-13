package mux

import (
	"encoding/binary" // For binary.BigEndian
	"time"

	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/common/buf"
	"github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/common/protocol"
	"github.com/v2fly/v2ray-core/v5/common/serial"
)

// Writer is responsible for packaging application layer data into Mux frames.
type Writer struct {
	dest             net.Destination
	writer           buf.Writer
	id               uint16
	followup         bool // True if this is a subsequent frame for an existing session
	transferType     protocol.TransferType
	priority         byte
	currentErrorCode uint16
	session          *Session // Mux.Pro: Reference to the associated Session for flow control
	globalID         GlobalID // Mux.Pro: GlobalID for UDP FullCone NAT (client-side New frame)

	// Mux.Pro: For server-side ResponseWriter, to embed UDP source in Keep frames
	responseUDPSource *net.Destination // Source address of the UDP response from target server
	responseGlobalID   GlobalID         // GlobalID to embed in server-to-client Keep frame
}

// NewWriter creates a new Writer for sending client-initiated Mux frames.
// id: Session ID.
// dest: Target destination.
// writer: Underlying buffer writer.
// transferType: Protocol transfer type (Stream or Packet).
// priority: Mux.Pro field, used to set the priority of a new connection.
// s: Mux.Pro parameter, points to the Session associated with this Writer for flow control.
// globalID: Mux.Pro parameter, GlobalID for UDP FullCone NAT (used in New frame).
func NewWriter(id uint16, dest net.Destination, writer buf.Writer, transferType protocol.TransferType, priority byte, s *Session, globalID GlobalID) *Writer {
	return &Writer{
		id:               id,
		dest:             dest,
		writer:           writer,
		followup:         false, // First frame will be SessionStatusNew
		transferType:     transferType,
		priority:         priority,
		currentErrorCode: ErrorCodeGracefulShutdown, // Mux.Pro: Default to graceful shutdown
		session:          s,                         // Mux.Pro: Associated Session
		globalID:         globalID,                  // Mux.Pro: GlobalID for client's New frame
	}
}

// NewResponseWriter creates a new Writer for sending server response Mux frames.
// id: Session ID.
// writer: Underlying buffer writer.
// transferType: Protocol transfer type (Stream or Packet).
// s: Mux.Pro parameter, points to the Session associated with this Writer for flow control.
// globalID: Mux.Pro parameter, GlobalID from client's New frame (to be included in server responses).
func NewResponseWriter(id uint16, writer buf.Writer, transferType protocol.TransferType, s *Session, globalID GlobalID) *Writer {
	return &Writer{
		id:               id,
		writer:           writer,
		followup:         true, // Response Writer always sends Keep frames
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

// SetResponseUDPInfo sets the UDP source information and GlobalID to be embedded
// in the next Keep frame sent from server to client.
func (w *Writer) SetResponseUDPInfo(globalID GlobalID, src net.Destination) {
	w.responseGlobalID = globalID
	w.responseUDPSource = &src
}

// getNextFrameMeta generates the metadata for the next Mux frame.
func (w *Writer) getNextFrameMeta() FrameMetadata {
	meta := FrameMetadata{
		SessionID: w.id,
		Target:    w.dest,
		// GlobalID is only set in New frames from client.
		// For Keep frames, it's part of Extra Data for UDP responses from server.
		// For client-to-server Keep frames, it's not in metadata.
	}

	if w.followup {
		meta.SessionStatus = SessionStatusKeep
	} else {
		w.followup = true
		meta.SessionStatus = SessionStatusNew
		meta.Priority = w.priority
		// For client-initiated New frames, GlobalID is part of FrameMetadata.
		if w.transferType == protocol.TransferTypePacket && w.globalID != (GlobalID{}) {
			meta.GlobalID = w.globalID
		}
	}

	return meta
}

// writeDataInternal is an internal function that actually writes data frames, without flow control logic.
// It constructs the full Extra Data payload including any UDP specific prefixes.
// packetDest: For UDP packets, this is the associated net.Destination (e.g., source for responses, target for requests).
// This parameter is used for client->server UDP (target) and server->client UDP (source).
func (w *Writer) writeDataInternal(dataMB buf.MultiBuffer, packetDest *net.Destination) error {
	meta := w.getNextFrameMeta()
	meta.Option.Set(OptionData) // Set OptionData bit, indicating data presence

	var extraDataPayload buf.MultiBuffer // This will hold the UDP specific prefix (if any)

	// Mux.Pro: Handle UDP specific Extra Data for Keep frames.
	// This applies to:
	// 1. Client sending UDP data: `packetDest` is `w.dest` (target address).
	// 2. Server sending UDP response: `packetDest` is `w.responseUDPSource` (actual source from target).
	if meta.SessionStatus == SessionStatusKeep && w.transferType == protocol.TransferTypePacket {
		if packetDest != nil {
			meta.Option.Set(OptionUDPData) // Set the OptionUDPData bit in metadata

			addrPrefix := buf.New()
			// UDP Extra Data format: 1 byte network type (0x02 for UDP), then address/port
			common.Must(addrPrefix.WriteByte(byte(TargetNetworkUDP)))
			// Use addrParser.WriteAddressPort from frame.go
			if err := addrParser.WriteAddressPort(addrPrefix, packetDest.Address, packetDest.Port); err != nil {
				addrPrefix.Release()
				return newError("failed to write UDP address/port to Extra Data").Base(err).AtWarning()
			}

			// If GlobalID is present (e.g., server sending response with client's GlobalID), append it here
			// For server-to-client responses, use w.responseGlobalID.
			if w.responseGlobalID != (GlobalID{}) {
				common.Must2(addrPrefix.Write(w.responseGlobalID[:]))
			}

			extraDataPayload = append(extraDataPayload, addrPrefix)
		}
	}

	// Construct the final frame with metadata, Extra Data (if any), and actual data payload.
	frame := buf.New()
	common.Must(meta.WriteTo(frame))

	// Write Extra Data length and content if present
	if !extraDataPayload.IsEmpty() {
		Must2(serial.WriteUint16(frame, uint16(extraDataPayload.Len())))
		Must2(frame.Write(extraDataPayload.Bytes()))
	}

	// Write actual data payload length and content
	Must2(serial.WriteUint16(frame, uint16(dataMB.Len())))
	Must2(frame.Write(dataMB.Bytes()))

	return w.writer.WriteMultiBuffer(buf.MultiBuffer{frame})
}

// WriteMultiBuffer implements buf.Writer interface, writing MultiBuffer to Mux frames.
// Includes Mux.Pro's credit-based flow control logic.
func (w *Writer) WriteMultiBuffer(mb buf.MultiBuffer) error {
	defer buf.ReleaseMulti(mb) // Ensure buffers are released

	if mb.IsEmpty() {
		// If no data, but it's the first frame for a new session (client side), send a New frame without data.
		// For Keep frames (followup is true), an empty MultiBuffer means no data to send, so just return.
		if !w.followup {
			return w.writeMetaOnly()
		}
		return nil
	}

	for !mb.IsEmpty() {
		var currentChunk buf.MultiBuffer // Used to process the current chunk being sent
		var packetDest *net.Destination   // For UDP packets, this will hold the associated address/port

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
			// For client-side outbound UDP, `w.dest` is the target.
			// For server-side inbound UDP responses, `w.responseUDPSource` is the actual source from the target.
			currentChunk = mb
			mb = nil // All data processed for this iteration

			if w.followup { // This is a Keep frame
				if w.responseUDPSource != nil { // Server sending response with specific source
					packetDest = w.responseUDPSource
				} else { // Client sending subsequent UDP data (target already in New frame)
					packetDest = &w.dest // Use the session's target destination
				}
			} else { // This is a New frame (client-side)
				packetDest = &w.dest // New frame always uses the session's target destination
			}
		}

		chunkLen := uint32(currentChunk.Len())
		if chunkLen == 0 { // Empty chunk, skip directly
			continue
		}

		// Only apply flow control if it's a client sending data.
		// Server sending data back to client is currently not credit-controlled by client in Mux.Pro.
		if !w.followup { // Client sending New or subsequent Keep frame
			for chunkLen > 0 { // Loop until the current chunk is fully sent
				// Mux.Pro: Before attempting to consume credit or wait, check if SessionManager is closing
				if w.session == nil || w.session.parent == nil || w.session.parent.Closed() {
					return newError("session or session manager closed while waiting for credit for session ", w.id).AtWarning()
				}

				consumed, ok := w.session.ConsumeCredit(chunkLen)
				if ok {
					// Successfully consumed credit, send the corresponding part of the data
					partToSend, remainingInChunk := buf.SplitSize(currentChunk, int32(consumed))
					if err := w.writeDataInternal(partToSend, packetDest); err != nil {
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
		} else { // Server sending data (followup is true)
			// Server-to-client data is not credit-controlled by client.
			if err := w.writeDataInternal(currentChunk, packetDest); err != nil {
				return err
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

	// If OptionData is set (meaning ErrorCode is present), write the error code as Extra Data.
	if meta.Option.Has(OptionData) {
		errorCodePayload := buf.New()
		common.Must(WriteUint16(errorCodePayload, w.currentErrorCode)) // ErrorCode is uint16 (2 bytes)
		defer errorCodePayload.Release()

		Must2(serial.WriteUint16(frame, uint16(errorCodePayload.Len()))) // Write Extra Data length
		Must2(frame.Write(errorCodePayload.Bytes()))                     // Write Extra Data content
	}

	// Attempt to write End frame, do not handle error as connection might be broken.
	// This is a best-effort send.
	_ = w.writer.WriteMultiBuffer(buf.MultiBuffer{frame})
	return nil
}
