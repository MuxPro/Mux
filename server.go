// server.go
package mux

import (
	"context"
	"encoding/binary"
	"io"
	"sync" // For sync.Map
	"sync/atomic" // For atomic.Int64
	"time"

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

// udpMappingEntry stores the original source and last activity time for a GlobalID.
type udpMappingEntry struct {
	OriginalSource net.Destination
	LastActivity   atomic.Int64 // UnixNano timestamp
}

const UDPGlobalIDTTL = time.Minute * 2         // 2 minutes TTL for UDP GlobalID mappings
const UDPGlobalIDCleanupInterval = time.Second * 30 // Cleanup every 30 seconds

// ServerWorker represents the server side of a Mux main connection.
type ServerWorker struct {
	dispatcher     routing.Dispatcher
	link           *transport.Link
	sessionManager *SessionManager
	// Mux.Pro: For UDP FullCone NAT mapping
	udpGlobalIDMap    sync.Map // map[GlobalID]*udpMappingEntry
	cancelCleanupCtx  context.CancelFunc
}

// NewServerWorker creates a new ServerWorker.
func NewServerWorker(ctx context.Context, d routing.Dispatcher, link *transport.Link) (*ServerWorker, error) {
	cleanupCtx, cancel := context.WithCancel(ctx)
	worker := &ServerWorker{
		dispatcher:     d,
		link:           link,
		sessionManager: NewSessionManager(),
		cancelCleanupCtx: cancel,
	}
	go worker.run(ctx)
	go worker.cleanupUDPGlobalIDMap(cleanupCtx) // Start cleanup goroutine
	return worker, nil
}

// cleanupUDPGlobalIDMap periodically cleans up expired UDP GlobalID mappings.
func (w *ServerWorker) cleanupUDPGlobalIDMap(ctx context.Context) {
	ticker := time.NewTicker(UDPGlobalIDCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			newError("UDP GlobalID map cleanup goroutine stopped.").WriteToLog()
			return
		case <-ticker.C:
			now := time.Now().UnixNano()
			w.udpGlobalIDMap.Range(func(key, value interface{}) bool {
				globalID := key.(GlobalID)
				entry := value.(*udpMappingEntry)
				if time.Duration(now-entry.LastActivity.Load()) > UDPGlobalIDTTL {
					newError("cleaning up expired UDP GlobalID mapping: ", globalID).WriteToLog()
					w.udpGlobalIDMap.Delete(globalID)
				}
				return true
			})
		}
	}
}

// handle handles data forwarding for sub-connections.
func handle(ctx context.Context, s *Session, output buf.Writer) {
	var packetDest *net.Destination // This will be the actual destination for UDP packets sent back to client

	// For UDP responses, the destination is the client's original source address.
	// This original source is stored in the Session struct (s.originalSource)
	// and also managed in the ServerWorker's udpGlobalIDMap via s.globalID.
	if s.transferType == protocol.TransferTypePacket && s.originalSource.IsValid() {
		packetDest = &s.originalSource
		newError("server sending UDP response to original source: ", *packetDest, " for GlobalID: ", s.globalID).WriteToLog()
	}

	// Pass the packetDest to NewResponseWriter, which will then pass it to Writer.writeDataInternal
	writer := NewResponseWriter(s.ID, output, s.transferType, s, s.globalID) // Pass s.globalID
	writer.SetPacketDestination(packetDest) // Set the specific packet destination for UDP responses

	if err := buf.Copy(s.input, writer); err != nil { // buf.Copy returns only error
		newError("session ", s.ID, " ends.").Base(err).WriteToLog(session.ExportIDToError(ctx))
		writer.SetErrorCode(ErrorCodeProtocolError)
		writer.Close()
		return
	}

	writer.Close() // Default is GracefulShutdown
	s.Close()
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
			if found {
				break
			}
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
	}
	if meta.Target.Network == net.Network_UDP {
		s.transferType = protocol.TransferTypePacket
		// Mux.Pro: Store GlobalID and original source for UDP FullCone NAT
		inbound := session.InboundFromContext(ctx)
		if inbound != nil && inbound.Source.IsValid() {
			s.originalSource = inbound.Source
			s.globalID = meta.GlobalID // GlobalID is already parsed from New frame metadata
			// Add/update mapping in the concurrent map
			w.udpGlobalIDMap.Store(s.globalID, &udpMappingEntry{
				OriginalSource: inbound.Source,
				LastActivity:   atomic.Int64(time.Now().UnixNano()),
			})
			newError("stored UDP GlobalID mapping: ", s.globalID, " -> ", s.originalSource).WriteToLog()
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
		// Determine transferType for closingWriter based on the incoming frame's target
		tt := protocol.TransferTypeStream
		if meta.Target.Network == net.Network_UDP { // If the original frame's target was UDP
			tt = protocol.TransferTypePacket
		}
		// Pass the GlobalID from the incoming frame's metadata to the closingWriter
		closingWriter := NewResponseWriter(meta.SessionID, w.link.Writer, tt, nil, meta.GlobalID)
		closingWriter.SetErrorCode(ErrorCodeProtocolError)
		closingWriter.Close()
		return buf.Copy(NewStreamReader(reader), buf.Discard) // Discard remaining data
	}

	var udpSrc net.Destination
	var globalID GlobalID
	var err error

	// Mux.Pro: If OptionUDPData is set, parse UDP source address and GlobalID from Extra Data.
	if meta.Option.Has(OptionUDPData) {
		udpSrc, globalID, err = readUDPMetaData(reader) // This reads from Extra Data
		if err != nil {
			newError("failed to read UDP metadata for session ", s.ID).Base(err).WriteToLog()
			// If metadata parsing fails, discard the remaining data and close session.
			closingWriter := NewResponseWriter(meta.SessionID, w.link.Writer, protocol.TransferTypeStream, nil, GlobalID{})
			closingWriter.SetErrorCode(ErrorCodeProtocolError)
			closingWriter.Close()
			return buf.Copy(NewStreamReader(reader), buf.Discard)
		}
		newError("received UDP data for session ", s.ID, " from ", udpSrc, " GlobalID: ", globalID).WriteToLog()

		// Update the GlobalID mapping with the latest activity and source
		if entry, loaded := w.udpGlobalIDMap.LoadOrStore(globalID, &udpMappingEntry{
			OriginalSource: udpSrc,
			LastActivity:   atomic.Int64(time.Now().UnixNano()),
		}); loaded {
			// If already exists, update LastActivity and potentially OriginalSource
			existingEntry := entry.(*udpMappingEntry)
			existingEntry.LastActivity.Store(time.Now().UnixNano())
			// Only update OriginalSource if it's different, or if we want to strictly follow the latest source.
			if !existingEntry.OriginalSource.Equals(udpSrc) {
				existingEntry.OriginalSource = udpSrc
				newError("updated UDP GlobalID mapping for ", globalID, " to new source: ", udpSrc).WriteToLog()
			}
		}
	}

	rr := s.NewReader(reader)
	var sc buf.SizeCounter // Declare a buf.SizeCounter instance
	err = buf.Copy(rr, s.output, buf.CountSize(&sc)) // Call buf.Copy and pass CountSize option
	copiedBytes := sc.Size // Get copied bytes from SizeCounter

	if err != nil && buf.IsWriteError(err) {
		newError("failed to write to downstream writer. closing session ", s.ID).Base(err).WriteToLog()
		// Pass the GlobalID from the original meta to the closingWriter, and use session's transferType
		closingWriter := NewResponseWriter(meta.SessionID, w.link.Writer, s.transferType, s, meta.GlobalID)
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
		// If the session has a GlobalID, remove it from the map upon session closure
		if s.globalID != (GlobalID{}) {
			w.udpGlobalIDMap.Delete(s.globalID)
			newError("removed UDP GlobalID mapping for session ", s.ID, " GlobalID: ", s.globalID).WriteToLog()
		}
		s.Close() // This will also remove the session from sessionManager
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
	defer w.cancelCleanupCtx() // Ensure cleanup goroutine is cancelled when worker stops

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
