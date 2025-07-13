package mux

import (
	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/common/buf"
	"github.com/v2fly/v2ray-core/v5/common/errors"
	"github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/common/protocol"
	"github.com/v2fly/v2ray-core/v5/common/serial"
)

type Writer struct {
	dest         net.Destination
	writer       buf.Writer
	id           uint16
	followup     bool
	hasError     bool // Mux.Cool field, Mux.Pro uses ErrorCode explicitly.
	transferType protocol.TransferType
	priority     byte // Mux.Pro Extension: Priority for new sessions (client only)
}

// NewWriter creates a new Writer for a new sub-connection (client side).
// This writer will send a SessionStatusNew frame first, followed by SessionStatusKeep frames.
func NewWriter(id uint16, dest net.Destination, writer buf.Writer, transferType protocol.TransferType, priority byte) *Writer {
	return &Writer{
		id:           id,
		dest:         dest,
		writer:       writer,
		followup:     false, // First frame will be SessionStatusNew
		transferType: transferType,
		priority:     priority,
	}
}

// NewResponseWriter creates a new Writer for responding to an existing sub-connection (server side).
// This writer will always send SessionStatusKeep frames.
func NewResponseWriter(id uint16, writer buf.Writer, transferType protocol.TransferType) *Writer {
	return &Writer{
		id:           id,
		writer:       writer,
		followup:     true, // First frame will be SessionStatusKeep
		transferType: transferType,
	}
}

func (w *Writer) getNextFrameMeta() FrameMetadata {
	meta := FrameMetadata{
		SessionID: w.id,
		Target:    w.dest,
		Priority:  w.priority, // Mux.Pro Extension: For SessionStatusNew
	}
	if w.followup {
		meta.SessionStatus = SessionStatusKeep
	} else {
		meta.SessionStatus = SessionStatusNew
		w.followup = true // Subsequent frames will be SessionStatusKeep
	}
	return meta
}

func (w *Writer) writeMetaOnly() error {
	meta := w.getNextFrameMeta()
	frame := buf.New()
	defer frame.Release()
	if err := meta.WriteTo(frame); err != nil {
		return err
	}
	// For meta-only frames, Extra Data Length is 0
	if _, err := serial.WriteUint16(frame, 0); err != nil {
		return err
	}
	return w.writer.WriteMultiBuffer(buf.MultiBuffer{frame})
}

// writeMetaWithFrame writes metadata and extra data (with length prefix) to the underlying writer.
func writeMetaWithFrame(writer buf.Writer, meta FrameMetadata, data buf.MultiBuffer) error {
	frame := buf.New()
	defer frame.Release() // Release frame buffer after use

	if err := meta.WriteTo(frame); err != nil {
		return newError("failed to write frame metadata").Base(err)
	}

	// Write Extra Data Length L
	if _, err := serial.WriteUint16(frame, uint16(data.Len())); err != nil {
		return newError("failed to write extra data length").Base(err)
	}

	// Check for total size limit (avoiding excessively large frames)
	// Add frame.Len() to data.Len() for total length check
	if frame.Len()+data.Len() > 64*1024*1024 { // Example: 64MB limit
		return errors.New("frame too large")
	}

	mb2 := make(buf.MultiBuffer, 0, len(data)+1) // +1 for the frame meta buffer
	mb2 = append(mb2, frame)
	mb2 = append(mb2, data...)
	return writer.WriteMultiBuffer(mb2)
}

func (w *Writer) writeData(mb buf.MultiBuffer) error {
	meta := w.getNextFrameMeta()
	meta.Option.Set(OptionData) // Indicate data presence

	return writeMetaWithFrame(w.writer, meta, mb)
}

// WriteMultiBuffer implements buf.Writer.
func (w *Writer) WriteMultiBuffer(mb buf.MultiBuffer) error {
	defer buf.ReleaseMulti(mb) // Ensure buffers are released

	if mb.IsEmpty() {
		return w.writeMetaOnly()
	}

	for !mb.IsEmpty() {
		var chunk buf.MultiBuffer
		if w.transferType == protocol.TransferTypeStream {
			mb, chunk = buf.SplitSize(mb, 8*1024) // Split into 8KB chunks for stream
		} else { // Packet transfer type
			mb, chunk = buf.SplitFirst(mb) // Take one full packet
		}
		if err := w.writeData(chunk); err != nil {
			return newError("failed to write data").Base(err)
		}
	}
	return nil
}

// Close closes the writer by sending an End frame with a specific error code.
func (w *Writer) Close(errorCode uint16) error { // Mux.Pro: Added errorCode parameter
	meta := FrameMetadata{
		SessionID:     w.id,
		SessionStatus: SessionStatusEnd,
	}
	if errorCode != ErrorCodeGracefulShutdown {
		meta.Option.Set(OptionData) // Mux.Pro: ErrorCode is Extra Data (though Mux.Cool used OptionError bit)
		meta.ErrorCode = errorCode
	}

	frame := buf.New()
	defer frame.Release()
	if err := meta.WriteTo(frame); err != nil {
		return newError("failed to write End frame metadata").Base(err)
	}
	// For End frame, Extra Data Length is 0 if no ErrorCode is present.
	// If ErrorCode is present in metadata (as per Mux.Pro spec clarification, it's part of metadata if Opt(D) is set for End frame),
	// there is no further extra data to write after the metadata length.
	// The `WriteTo` method for `End` frame now handles writing `ErrorCode` if `OptionData` is set for it.
	if _, err := serial.WriteUint16(frame, 0); err != nil { // No Extra Data beyond ErrorCode (which is in metadata)
		return newError("failed to write Extra Data Length for End frame").Base(err)
	}

	return w.writer.WriteMultiBuffer(buf.MultiBuffer{frame})
}

// WriteKeepAliveFrame writes a KeepAlive frame to the given writer.
// Mux.Pro: KeepAlive frames MUST NOT carry any Extra Data, and Opt MUST be 0x00.
func WriteKeepAliveFrame(writer buf.Writer) error {
	meta := FrameMetadata{
		SessionID:     0, // Random or arbitrary 2-byte value. 0 is fine.
		SessionStatus: SessionStatusKeepAlive,
		Option:        0x00, // Mux.Pro: Must be 0x00
	}

	frame := buf.New()
	defer frame.Release()
	if err := meta.WriteTo(frame); err != nil {
		return newError("failed to write KeepAlive frame metadata").Base(err)
	}

	// For KeepAlive frames, Extra Data Length MUST be 0
	if _, err := serial.WriteUint16(frame, 0); err != nil {
		return newError("failed to write Extra Data Length for KeepAlive frame").Base(err)
	}

	return writer.WriteMultiBuffer(buf.MultiBuffer{frame})
}
