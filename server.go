package mux

import (
	"context"
	"encoding/binary"
	"io"
	"sync" // For sync.Map

	core "github.com/v2fly/v2ray-core/v5"
	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/common/buf"
	"github.com/v2fly/v2ray-core/v5/common/errors"
	"github.com/v2fly/v2ray-core/v5/common/log"
	"github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/common/protocol"
	"github.com/v2fly/v2ray-core/v5/common/serial"
	"github.com/v2fly/v2ray-core/v5/common/session"
	"github.com/v2fly/v2ray-core/v5/features/routing"
	"github.com/v2fly/v2ray-core/v5/transport"
	"github.com/v2fly/v2ray-core/v5/transport/pipe"
)

// Server implements Mux.Pro server-side logic.
type Server struct {
	dispatcher routing.Dispatcher
}

// NewServer creates a new Mux server instance.
func NewServer(ctx context.Context) *Server {
	s := &Server{}
	core.RequireFeatures(ctx, func(d routing.Dispatcher) {
		s.dispatcher = d
	})
	return s
}

// Type returns the type of the server.
func (s *Server) Type() interface{} {
	return s.dispatcher.Type()
}

// Dispatch handles inbound connections.
func (s *Server) Dispatch(ctx context.Context, dest net.Destination) (*transport.Link, error) {
	// Mux.Pro: Check if the target address is "mux.pro"
	if dest.Address != muxProAddress {
		return s.dispatcher.Dispatch(ctx, dest)
	}

	opts := pipe.OptionsFromContext(ctx)
	uplinkReader, uplinkWriter := pipe.New(opts...)
	downlinkReader, downlinkWriter := pipe.New(opts...)

	_, err := NewServerWorker(ctx, s.dispatcher, &transport.Link{
		Reader: uplinkReader,
		Writer: downlinkWriter,
	})
	if err != nil {
		return nil, err
	}

	return &transport.Link{Reader: downlinkReader, Writer: uplinkWriter}, nil
}

// Start starts the server.
func (s *Server) Start() error {
	return nil
}

// Close closes the server.
func (s *Server) Close() error {
	return nil
}

// ServerWorker represents the server side of a Mux main connection.
type ServerWorker struct {
	dispatcher     routing.Dispatcher
	link           *transport.Link
	sessionManager *SessionManager
	// Mux.Pro: Mapping from GlobalID to client's original UDP source address for FullCone NAT
	globalIDToClientAddr sync.Map // map[GlobalID]net.Destination
}

// NewServerWorker creates a new ServerWorker.
func NewServerWorker(ctx context.Context, d routing.Dispatcher, link *transport.Link) (*ServerWorker, error) {
	worker := &ServerWorker{
		dispatcher:     d,
		link:           link,
		sessionManager: NewSessionManager(),
		globalIDToClientAddr: sync.Map{}, // Initialize the map
	}
	go worker.run(ctx) // Start the main loop
	return worker, nil
}

// handle handles data forwarding for sub-connections.
func handle(ctx context.Context, s *Session, output buf.Writer) {
	// For server-side, the writer will use s.GlobalID and s.ClientUDPOrigin
	// to send back UDP responses with correct source/GlobalID.
	writer := NewResponseWriter(s.ID, output, s.transferType, s, s.GlobalID) // Pass GlobalID from session
	defer s.Close()
	defer writer.Close()

	rr := s.NewReader(s.input.(*buf.BufferedReader)) // Assuming s.input is BufferedReader
	// In server-side handle, s.input is the link to the actual target server.
	// If it's a UDP connection to the target, the buffers read from s.input
	// might contain UDP source info.
	for {
		mb, err := rr.ReadMultiBuffer()
		if err != nil {
			if errors.Cause(err) == io.EOF {
				break
			}
			newError("failed to read from target server for session ", s.ID).Base(err).WriteToLog(session.ExportIDToError(ctx))
			writer.SetErrorCode(ErrorCodeRemoteDisconnect)
			break
		}
		if mb.IsEmpty() {
			mb.ReleaseMulti()
			continue
		}

		// If this is a UDP session, we need to ensure the source address of the
		// response from the target server is captured and passed back to the client.
		// The `PacketReader` (rr) should have set `b.UDPInfo` if it's a UDP response.
		if s.transferType == protocol.TransferTypePacket {
			for _, b := range mb {
				if b.UDPInfo != nil && b.UDPInfo.Network == net.Network_UDP {
					// This is a UDP response from the target server.
					// We need to send it back to the client with its original source
					// (the UDPInfo from the buffer) and the session's GlobalID.
					// The writer.writeDataInternal will handle embedding this into Extra Data.
					newError("Server sending UDP response from ", b.UDPInfo.Address, ":", b.UDPInfo.Port, " with GlobalID: ", s.GlobalID).WriteToLog()
					// Temporarily set writer's dest to the actual UDP source for this packet
					// This is a bit of a hack; ideally, writer.WriteMultiBuffer should take
					// a per-buffer destination for UDP.
					originalWriterDest := writer.dest
					writer.dest = *b.UDPInfo // Use the actual source of the UDP response as the destination for the Mux frame
					writer.globalID = s.GlobalID // Ensure GlobalID is set for the writer
					if writeErr := writer.WriteMultiBuffer(buf.MultiBuffer{b}); writeErr != nil {
						newError("failed to write UDP response to client").Base(writeErr).WriteToLog()
						writer.dest = originalWriterDest // Restore
						writer.SetErrorCode(ErrorCodeProtocolError)
						return
					}
					writer.dest = originalWriterDest // Restore
				} else {
					// This should not happen for UDP sessions if the reader is correctly setting UDPInfo.
					newError("Warning: UDP session received non-UDP-info buffer").WriteToLog()
					if writeErr := writer.WriteMultiBuffer(buf.MultiBuffer{b}); writeErr != nil {
						newError("failed to write non-UDP-info buffer to client").Base(writeErr).WriteToLog()
						writer.SetErrorCode(ErrorCodeProtocolError)
						return
					}
				}
			}
		} else { // Stream transfer type
			if err := writer.WriteMultiBuffer(mb); err != nil {
				newError("failed to write stream data to client").Base(err).WriteToLog()
				writer.SetErrorCode(ErrorCodeProtocolError)
				return
			}
		}
		mb.ReleaseMulti()
	}
}

// ActiveConnections returns the number of active connections.
func (w *ServerWorker) ActiveConnections() uint32 {
	return uint32(w.sessionManager.Size())
}

// Closed checks if the worker is closed.
func (w *ServerWorker) Closed() bool {
	return w.sessionManager.Closed()
}

// negotiate handles Mux.Pro version negotiation.
func (w *ServerWorker) negotiate(reader *buf.BufferedReader) error {
	// 1. Read client's negotiation request
	var meta FrameMetadata
	if err := meta.Unmarshal(reader); err != nil {
		return newError("failed to read negotiation request metadata").Base(err)
	}

	// Check if ID and status conform to version negotiation frame requirements
	if meta.SessionID != 0 || meta.SessionStatus != SessionStatusNegotiateVersion {
		return newError("unexpected first frame. expected negotiation, got: ID=", meta.SessionID, " Status=", meta.SessionStatus)
	}

	// Check if negotiation request contains Extra Data
	if !meta.Option.Has(OptionData) {
		return newError("negotiation request has no data")
	}

	// Read the length of client request's Extra Data
	dataLen, err := serial.ReadUint16(reader)
	if err != nil {
		return newError("failed to read negotiation request data length").Base(err)
	}

	// Read client request's Extra Data
	requestPayload := buf.New()
	defer requestPayload.Release()
	if _, err := requestPayload.ReadFullFrom(reader, int32(dataLen)); err != nil {
		return err
	}

	// Parse client version list: [1-byte version count N] [N * 4-byte version numbers]
	count := requestPayload.Byte(0)
	requestPayload.Advance(1) // Consume version count byte

	var clientVersions []uint32
	for i := 0; i < int(count); i++ {
		if requestPayload.Len() < 4 {
			return newError("insufficient data for version list")
		}
		clientVersions = append(clientVersions, binary.BigEndian.Uint32(requestPayload.Bytes()))
		requestPayload.Advance(4)
	}

	var negotiatedVersion uint32
	var found bool
	supportedVersions := []uint32{Version} // Server's supported version list (currently only Mux.Pro 0.0)

	// Iterate through client versions to find the highest common version
	for _, clientVer := range clientVersions {
		for _, serverVer := range supportedVersions {
			if clientVer == serverVer {
				negotiatedVersion = serverVer
				found = true
				break
			}
		}
		if found {
			break
		}
	}

	if !found {
		return newError("no compatible version found. client versions: ", clientVersions)
	}

	// 3. Send successful negotiation response
	responseMeta := FrameMetadata{
		SessionID:     0,
		SessionStatus: SessionStatusNegotiateVersion,
		Option:        OptionData, // OptionData must be set as response contains negotiated version
	}

	frame := buf.New()
	common.Must(responseMeta.WriteTo(frame))

	// Construct response Extra Data: [1-byte version count N=1] [1 * 4-byte negotiated version number]
	versionsPayload := buf.New()
	defer versionsPayload.Release()
	common.Must(versionsPayload.WriteByte(1)) // Version count N = 1
	common.Must(WriteUint32(versionsPayload, negotiatedVersion))

	Must2(serial.WriteUint16(frame, uint16(versionsPayload.Len()))) // Write Extra Data length
	Must2(frame.Write(versionsPayload.Bytes()))                     // Write Extra Data content

	if err := w.link.Writer.WriteMultiBuffer(buf.MultiBuffer{frame}); err != nil {
		return newError("failed to write negotiation response").Base(err)
	}

	newError("Mux.Pro negotiation succeeded, version ", negotiatedVersion).WriteToLog()
	return nil
}

// handleStatusKeepAlive handles KeepAlive frames.
func (w *ServerWorker) handleStatusKeepAlive(meta *FrameMetadata, reader *buf.BufferedReader) error {
	// Mux.Pro: KeepAlive frame's Opt must be 0x00 and carry no data.
	if meta.Option != 0 {
		return newError("protocol error: KeepAlive frame with non-zero option: ", meta.Option)
	}
	// KeepAlive frames should not have Extra Data, no extra handling needed here.
	return nil
}

// handleCreditUpdate handles CreditUpdate frames.
func (w *ServerWorker) handleCreditUpdate(meta *FrameMetadata, reader *buf.BufferedReader) error {
	if !meta.Option.Has(OptionData) {
		return newError("CreditUpdate frame has no data")
	}

	s, found := w.sessionManager.Get(meta.SessionID)
	if !found {
		return buf.Copy(NewStreamReader(reader), buf.Discard) // Discard credit update for unknown session
	}

	// Read Credit Increment (Extra Data)
	dataLen, err := serial.ReadUint16(reader)
	if err != nil {
		return newError("failed to read CreditUpdate data length").Base(err)
	}
	if dataLen != 4 { // Mux.Pro spec states Credit Increment is 4 bytes (uint32)
		return newError("invalid CreditUpdate data length: ", dataLen)
	}

	payload := buf.New()
	defer payload.Release()
	if _, err := payload.ReadFullFrom(reader, int32(dataLen)); err != nil {
		return newError("failed to read CreditUpdate payload").Base(err)
	}

	increment := binary.BigEndian.Uint32(payload.Bytes())
	s.GrantCredit(increment) // Increase session's send credit

	newError("session ", s.ID, " granted credit: ", increment).WriteToLog()
	return nil
}

// handleStatusNew handles New frames, creating new sub-connections.
func (w *ServerWorker) handleStatusNew(ctx context.Context, meta *FrameMetadata, reader *buf.BufferedReader) error {
	// Mux.Pro: Record new connection's priority
	newError("received request for ", meta.Target, " with priority ", meta.Priority).WriteToLog(session.ExportIDToError(ctx))
	{
		msg := &log.AccessMessage{
			To:     meta.Target,
			Status: log.AccessAccepted,
			Reason: "",
		}
		if inbound := session.InboundFromContext(ctx); inbound != nil && inbound.Source.IsValid() {
			msg.From = inbound.Source
			msg.Email = inbound.User.Email
		}
		ctx = log.ContextWithAccessMessage(ctx, msg)
	}
	link, err := w.dispatcher.Dispatch(ctx, meta.Target)
	if err != nil {
		if meta.Option.Has(OptionData) { // If New frame contains initial data, discard it
			buf.Copy(NewStreamReader(reader), buf.Discard)
		}
		return newError("failed to dispatch request.").Base(err)
	}

	s := &Session{
		input:        link.Reader,
		output:       link.Writer,
		parent:       w.sessionManager,
		ID:           meta.SessionID,
		transferType: protocol.TransferTypeStream,
		sendCredit: DefaultInitialCredit, // Mux.Pro: Server-side new sessions also need initial credit
		creditUpdate: make(chan struct{}, 1),
		receivedBytes: 0,
		GlobalID: meta.GlobalID, // Mux.Pro: Store GlobalID from client's New frame
	}

	if meta.Target.Network == net.Network_UDP {
		s.transferType = protocol.TransferTypePacket
		// Mux.Pro: Store client's original UDP source address for FullCone NAT
		inbound := session.InboundFromContext(ctx)
		if inbound != nil && inbound.Source.IsValid() && inbound.Source.Network == net.Network_UDP {
			w.globalIDToClientAddr.Store(meta.GlobalID, inbound.Source)
			s.ClientUDPOrigin = &inbound.Source // Store client's original UDP source in session
		} else {
			newError("Warning: UDP session initiated without valid inbound UDP source for GlobalID: ", meta.GlobalID).WriteToLog()
		}
	}
	
	w.sessionManager.Add(s)
	go handle(ctx, s, w.link.Writer) // Start data forwarding for sub-connection
	if !meta.Option.Has(OptionData) {
		return nil
	}

	rr := s.NewReader(reader)
	if err := buf.Copy(rr, s.output); err != nil {
		buf.Copy(rr, buf.Discard) // Discard remaining data
		common.Interrupt(s.input)
		return s.Close()
	}
	return nil
}

// handleStatusKeep handles Keep frames (data transfer).
func (w *ServerWorker) handleStatusKeep(meta *FrameMetadata, reader *buf.BufferedReader) error {
	if !meta.Option.Has(OptionData) {
		return nil
	}

	s, found := w.sessionManager.Get(meta.SessionID)
	if !found {
		// Pass a zero GlobalID for now; actual GlobalID will be extracted from Keep frame in future stages
		closingWriter := NewResponseWriter(meta.SessionID, w.link.Writer, protocol.TransferTypeStream, nil, GlobalID{}) 
		closingWriter.SetErrorCode(ErrorCodeProtocolError)
		closingWriter.Close()
		return buf.Copy(NewStreamReader(reader), buf.Discard)
	}

	rr := s.NewReader(reader)
	var sc buf.SizeCounter // Declare a buf.SizeCounter instance
	err := buf.Copy(rr, s.output, buf.CountSize(&sc)) // Call buf.Copy and pass CountSize option
	copiedBytes := sc.Size // Get copied bytes from SizeCounter

	if err != nil && buf.IsWriteError(err) {
		newError("failed to write to downstream writer. closing session ", s.ID).Base(err).WriteToLog()
		// Pass a zero GlobalID for now
		closingWriter := NewResponseWriter(meta.SessionID, w.link.Writer, protocol.TransferTypeStream, s, GlobalID{})
		closingWriter.SetErrorCode(ErrorCodeProtocolError)
		closingWriter.Close()
		drainErr := buf.Copy(rr, buf.Discard) // Discard remaining data
		common.Interrupt(s.input)
		s.Close()
		return drainErr
	}

	// Mux.Pro: Receiver logic - send CreditUpdate frame
	s.AddReceivedBytes(uint32(copiedBytes))
	if s.GetReceivedBytes() >= CreditUpdateThreshold {
		newError("sending credit update for session ", s.ID, ", increment: ", DefaultInitialCredit).WriteToLog()
		// Construct CreditUpdate frame
		creditMeta := FrameMetadata{
			SessionID:     s.ID,
			SessionStatus: SessionStatusCreditUpdate,
			Option:        OptionData, // OptionData must be set as credit increment is Extra Data
		}
		creditFrame := buf.New()
		common.Must(creditMeta.WriteTo(creditFrame))

		// Construct Extra Data: 4-byte credit increment
		creditPayload := buf.New()
		common.Must(WriteUint32(creditPayload, DefaultInitialCredit)) // Add DefaultInitialCredit credit
		defer creditPayload.Release()

		Must2(serial.WriteUint16(creditFrame, uint16(creditPayload.Len())))
		Must2(creditFrame.Write(creditPayload.Bytes()))

		// Attempt to send CreditUpdate frame
		if writeErr := w.link.Writer.WriteMultiBuffer(buf.MultiBuffer{creditFrame}); writeErr != nil {
			newError("failed to send CreditUpdate frame for session ", s.ID).Base(writeErr).WriteToLog()
		} else {
			s.ResetReceivedBytes() // Reset count after successful send
		}
	}

	return err
}

// handleStatusEnd handles End frames (closing sub-connection).
func (w *ServerWorker) handleStatusEnd(meta *FrameMetadata, reader *buf.BufferedReader) error {
	if s, found := w.sessionManager.Get(meta.SessionID); found {
		// Mux.Pro: Use ErrorCode instead of OptionError to determine close reason.
		if meta.ErrorCode != ErrorCodeGracefulShutdown {
			newError("session ", s.ID, " ended with error code: ", meta.ErrorCode).WriteToLog()
			common.Interrupt(s.input)
			common.Interrupt(s.output)
		}
		s.Close()
		// Mux.Pro: Remove GlobalID mapping when session ends
		if s.transferType == protocol.TransferTypePacket && s.GlobalID != (GlobalID{}) {
			w.globalIDToClientAddr.Delete(s.GlobalID)
		}
	}
	if meta.Option.Has(OptionData) { // If End frame unexpectedly contains data, discard it
		return buf.Copy(NewStreamReader(reader), buf.Discard)
	}
	return nil
}

// handleFrame handles received Mux frames.
func (w *ServerWorker) handleFrame(ctx context.Context, reader *buf.BufferedReader) error {
	var meta FrameMetadata
	err := meta.Unmarshal(reader) // Parse frame metadata
	if err != nil {
		return newError("failed to read metadata").Base(err)
	}

	switch meta.SessionStatus {
	case SessionStatusKeepAlive:
		err = w.handleStatusKeepAlive(&meta, reader)
	case SessionStatusEnd:
		err = w.handleStatusEnd(&meta, reader)
	case SessionStatusNew:
		err = w.handleStatusNew(ctx, &meta, reader)
	case SessionStatusKeep:
		err = w.handleStatusKeep(&meta, reader)
	case SessionStatusCreditUpdate: // Mux.Pro: Handle CreditUpdate frame
		err = w.handleCreditUpdate(&meta, reader)
	case SessionStatusNegotiateVersion:
		// This frame should not be received after negotiation, treat as protocol error
		err = newError("unexpected NegotiateVersion frame after initial negotiation")
	default:
		status := meta.SessionStatus
		return newError("unknown status: ", status).AtError()
	}

	if err != nil {
		return newError("failed to process data").Base(err)
	}
	return nil
}

// run is the main loop of ServerWorker, responsible for reading and processing inbound Mux frames.
func (w *ServerWorker) run(ctx context.Context) {
	input := w.link.Reader
	reader := &buf.BufferedReader{Reader: input}

	defer w.sessionManager.Close()

	// Mux.Pro: Perform version negotiation first, before processing any other frames.
	if err := w.negotiate(reader); err != nil {
		newError("Mux.Pro negotiation failed").Base(err).WriteToLog(session.ExportIDToError(ctx))
		common.Interrupt(input) // Negotiation failed, interrupt underlying connection
		return
	}

	for {
		select {
		case <-ctx.Done(): // Context cancellation signal
			return
		default:
			err := w.handleFrame(ctx, reader) // Process next frame
			if err != nil {
				if errors.Cause(err) != io.EOF {
					newError("worker run loop ended").Base(err).WriteToLog(session.ExportIDToError(ctx))
					common.Interrupt(input) // Error occurred, interrupt underlying connection
				}
				return
			}
		}
	}
}
