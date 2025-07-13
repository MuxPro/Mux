package mux

import (
	"context"
	"io"
	"time"

	core "github.com/v2fly/v2ray-core/v5"
	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/common/buf"
	"github.com/v2fly/v2ray-core/v5/common/errors"
	"github.com/v2fly/v2ray-core/v5/common/log"
	"github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/common/protocol"
	"github.com/v2fly/v2ray-core/v5/common/session"
	"github.com/v2fly/v2ray-core/v5/features/routing"
	"github.com/v2fly/v2ray-core/v5/transport"
	"github.com/v2fly/v2ray-core/v5/transport/pipe"
)

type Server struct {
	dispatcher routing.Dispatcher
}

// NewServer creates a new mux.Server.
func NewServer(ctx context.Context) *Server {
	s := &Server{}
	core.RequireFeatures(ctx, func(d routing.Dispatcher) {
		s.dispatcher = d
	})
	return s
}

// Type implements common.HasType.
func (s *Server) Type() interface{} {
	return s.dispatcher.Type()
}

// Dispatch implements routing.Dispatcher.
func (s *Server) Dispatch(ctx context.Context, dest net.Destination) (*transport.Link, error) {
	if dest.Address.Equals(muxProAddress) && dest.Port == muxProPort {
		return s.startServerWorker(ctx)
	}
	return s.dispatcher.Dispatch(ctx, dest)
}

func (s *Server) startServerWorker(ctx context.Context) (*transport.Link, error) {
	uplink, downlink := pipe.New(pipe.WithContext(ctx))
	worker := NewServerWorker(ctx, s.dispatcher, downlink)
	go worker.run(ctx)
	return uplink, nil
}

type ServerWorker struct {
	dispatcher     routing.Dispatcher
	link           *transport.Link
	sessionManager *SessionManager
}

func NewServerWorker(ctx context.Context, d routing.Dispatcher, link *transport.Link) *ServerWorker {
	return &ServerWorker{
		dispatcher:     d,
		link:           link,
		sessionManager: NewSessionManager(),
	}
}

func (w *ServerWorker) handle(ctx context.Context, s *Session, output buf.Writer) {
	defer common.Close(s.input)
	defer common.Close(output)
	defer s.Close()

	// Mux.Pro: Credit-based flow control for sending data to client
	// Initial credit is set on session creation.
	// We need to continuously send CreditUpdate frames back to the client.
	// For simplicity, we assume an initial default credit and then
	// periodically grant more credit or upon consuming a certain amount of data.
	// A more robust implementation would track bytes read from s.input and
	// send CreditUpdate proportional to consumption.
	go func() {
		creditGrantInterval := 500 * time.Millisecond // Example interval
		creditGrantAmount := uint32(16 * 1024)        // Example: Grant 16KB at a time

		ticker := time.NewTicker(creditGrantInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !w.sessionManager.Closed() {
					err := WriteCreditUpdateFrame(w.link.Writer, s.ID, creditGrantAmount)
					if err != nil {
						newError("failed to send credit update for session ", s.ID).Base(err).WriteToLog(log.DefaultLogger())
						return
					}
				}
			}
		}
	}()

	_, err := buf.Copy(s.input, output)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			newError("session ", s.ID, " copy ended with error").Base(err).WriteToLog(log.DefaultLogger())
			// Mux.Pro: Send End frame with appropriate error code
			common.Close(NewResponseWriter(s.ID, w.link.Writer, s.transferType).Close(ErrorCodeRemoteDisconnect))
		}
	} else {
		// Mux.Pro: Send graceful End frame
		common.Close(NewResponseWriter(s.ID, w.link.Writer, s.transferType).Close(ErrorCodeGracefulShutdown))
	}
}

func (w *ServerWorker) handleStatusKeepAlive(meta *FrameMetadata, reader *buf.BufferedReader) error {
	// Mux.Pro: Opt MUST be 0x00. No Extra Data.
	if meta.Option != 0x00 {
		return newError("Mux.Pro KeepAlive Opt must be 0x00").AtError()
	}
	// As per Mux.Pro, KeepAlive MUST NOT carry any Extra Data.
	// If OptionData was mistakenly set, it's a protocol error.
	if meta.Option.Has(OptionData) {
		return newError("Mux.Pro KeepAlive must not have Extra Data").AtError()
	}
	return nil
}

func (w *ServerWorker) handleStatusEnd(meta *FrameMetadata, reader *buf.BufferedReader) error {
	s, found := w.sessionManager.Get(meta.SessionID)
	if found {
		// Mux.Pro: Read ErrorCode from metadata
		// The UnmarshalFromBuffer already handled reading ErrorCode if OptionData is set.
		newError("Session ", meta.SessionID, " closed with error code: ", meta.ErrorCode).WriteToLog(log.DefaultLogger())
		common.Interrupt(s.input)
		common.Interrupt(s.output)
		s.Close()
	}

	// Mux.Pro: Any extra data in End frame should be discarded
	if meta.Option.Has(OptionData) {
		// This handles the case if OptionData was set but ErrorCode field wasn't processed correctly
		// or if there's truly extra data (protocol violation).
		return buf.Copy(NewStreamReader(reader), buf.Discard)
	}
	return nil
}

func (w *ServerWorker) handleStatusNew(ctx context.Context, meta *FrameMetadata, reader *buf.BufferedReader) error {
	// Mux.Pro: Priority P is now available in meta.Priority.
	// Currently, this implementation doesn't use Priority for scheduling, but it's parsed.
	newError("New session ", meta.SessionID, " to ", meta.Target, " with priority ", meta.Priority).WriteToLog(log.DefaultLogger())

	s := &Session{
		ID:           meta.SessionID,
		parent:       w.sessionManager,
		transferType: protocol.TransferTypeStream, // Default to stream, as V2Ray mostly uses TCP
		credit:       initialSessionCredit,        // Mux.Pro: Initialize session credit
	}
	w.sessionManager.Add(s)

	sessionCtx := session.ContextWithInbound(context.Background(), session.NewInbound(&net.Destination{
		Address: meta.Target.Address,
		Port:    meta.Target.Port,
		Network: meta.Target.Network,
	}))

	link, err := w.dispatcher.Dispatch(sessionCtx, meta.Target)
	if err != nil {
		common.Close(s.Close())
		common.Close(NewResponseWriter(meta.SessionID, w.link.Writer, s.transferType).Close(ErrorCodeDestinationUnreachable))
		return newError("failed to dispatch connection for session ", meta.SessionID).Base(err)
	}
	s.input = link.Reader
	s.output = link.Writer

	go w.handle(sessionCtx, s, NewResponseWriter(s.ID, w.link.Writer, s.transferType))

	if meta.Option.Has(OptionData) {
		// Mux.Pro: Consume initial credit for the data being sent
		dataLen := reader.Buffer().Len() // Total length of data in the current frame
		if !s.ConsumeCredit(uint32(dataLen)) {
			// This indicates client sent data before enough credit was granted
			// It's a protocol violation or flow control misstep.
			newError("session ", s.ID, " received initial data without enough credit. Discarding.").WriteToLog(log.DefaultLogger())
			common.Close(NewResponseWriter(s.ID, w.link.Writer, s.transferType).Close(ErrorCodeProtocolError))
			common.Close(s.Close())
			return buf.Copy(NewStreamReader(reader), buf.Discard)
		}
		_, err := buf.Copy(NewStreamReader(reader), s.output)
		if err != nil {
			common.Close(NewResponseWriter(s.ID, w.link.Writer, s.transferType).Close(ErrorCodeRemoteDisconnect))
			common.Close(s.Close())
			return newError("failed to write initial data for session ", s.ID).Base(err)
		}
	}

	return nil
}

func (w *ServerWorker) handleStatusKeep(meta *FrameMetadata, reader *buf.BufferedReader) error {
	if !meta.Option.Has(OptionData) {
		return nil
	}

	s, found := w.sessionManager.Get(meta.SessionID)
	if !found {
		// Mux.Pro: Remote trying to send data to a non-existent session.
		// Send End frame to notify them to close.
		newError("received data for unknown session ", meta.SessionID, ". Sending End frame.").WriteToLog(log.DefaultLogger())
		common.Close(NewResponseWriter(meta.SessionID, w.link.Writer, protocol.TransferTypeStream).Close(ErrorCodeProtocolError))
		return buf.Copy(NewStreamReader(reader), buf.Discard)
	}

	// Mux.Pro: Flow control: Consume credit for incoming data
	dataBuf := reader.Buffer()
	dataLen := dataBuf.Len()
	if !s.HasCredit(uint32(dataLen)) {
		newError("session ", s.ID, " received data without enough credit. Available: ", s.GetCredit(), ", Needed: ", dataLen).WriteToLog(log.DefaultLogger())
		// This is a protocol violation. Close the session.
		common.Close(NewResponseWriter(s.ID, w.link.Writer, s.transferType).Close(ErrorCodeProtocolError))
		common.Close(s.Close())
		return buf.Copy(NewStreamReader(reader), buf.Discard)
	}
	s.ConsumeCredit(uint32(dataLen)) // Consume before actual write

	_, err := buf.Copy(NewStreamReader(reader), s.output)
	if err != nil {
		newError("failed to write data for session ", meta.SessionID).Base(err).WriteToLog(log.DefaultLogger())
		// Mux.Pro: Send End frame with appropriate error code
		common.Close(NewResponseWriter(s.ID, w.link.Writer, s.transferType).Close(ErrorCodeRemoteDisconnect))
		common.Close(s.Close())
		return err
	}
	return nil
}

// handleStatusCreditUpdate handles incoming CreditUpdate frames from the client.
func (w *ServerWorker) handleStatusCreditUpdate(meta *FrameMetadata, reader *buf.BufferedReader) error {
	_, increment, err := ReadCreditUpdateFrame(reader)
	if err != nil {
		return newError("failed to read CreditUpdate frame").Base(err).AtError()
	}

	s, found := w.sessionManager.Get(meta.SessionID)
	if !found {
		newError("received credit update for unknown session ", meta.SessionID, ". Discarding.").WriteToLog(log.DefaultLogger())
		return nil // Just discard for unknown session
	}

	s.AddCredit(increment)
	newError("Session ", s.ID, " credit updated by ", increment, ". New credit: ", s.GetCredit()).WriteToLog(log.DefaultLogger())
	return nil
}

func (w *ServerWorker) handleFrame(ctx context.Context, reader *buf.BufferedReader) error {
	var meta FrameMetadata
	err := meta.Unmarshal(reader)
	if err != nil {
		return newError("failed to read metadata").Base(err).AtError()
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
	case SessionStatusCreditUpdate: // Mux.Pro: New frame type
		err = w.handleStatusCreditUpdate(&meta, reader)
	default:
		status := meta.SessionStatus
		return newError("unknown status: ", status).AtError()
	}

	if err != nil {
		return newError("failed to process data").Base(err).AtError()
	}
	return nil
}

func (w *ServerWorker) run(ctx context.Context) {
	input := w.link.Reader
	reader := &buf.BufferedReader{Reader: input}

	defer w.sessionManager.Close()
	defer common.Close(w.link.Writer)
	defer common.Close(w.link.Reader)

	// Mux.Pro: Version Negotiation (REQUIRED)
	newError("Server: Starting Mux.Pro version negotiation.").WriteToLog(log.DefaultLogger())
	clientVersions, err := w.negotiateVersion(ctx, reader)
	if err != nil {
		newError("Server: Mux.Pro version negotiation failed: ", err).WriteToLog(log.DefaultLogger())
		return // Close connection on negotiation failure
	}
	newError("Server: Mux.Pro client supported versions: ", clientVersions).WriteToLog(log.DefaultLogger())

	// Select the highest common version. For now, we only support MuxProVersion.
	selectedVersion := uint33(0)
	for _, v := range clientVersions {
		if v == MuxProVersion {
			selectedVersion = MuxProVersion
			break
		}
	}

	if selectedVersion == 0 { // No common version found
		newError("Server: No common Mux.Pro version found. Closing connection.").WriteToLog(log.DefaultLogger())
		// Optionally send an error frame before closing, though spec says just close.
		return
	}

	newError("Server: Mux.Pro version negotiated: ", selectedVersion).WriteToLog(log.DefaultLogger())
	// Send server's selected version back
	if err := WriteNegotiateVersionFrame(w.link.Writer, []uint32{selectedVersion}); err != nil {
		newError("Server: Failed to send negotiated Mux.Pro version: ", err).Base(err).WriteToLog(log.DefaultLogger())
		return
	}

	newError("Server: Mux.Pro version negotiation successful.").WriteToLog(log.DefaultLogger())

	// Main loop for Mux.Pro frame processing
	for {
		if w.sessionManager.Closed() {
			break
		}

		select {
		case <-ctx.Done():
			newError("Server worker context done. Closing.").WriteToLog(log.DefaultLogger())
			return
		default:
			err = w.handleFrame(ctx, reader)
			if err != nil {
				if errors.Is(err, io.EOF) {
					newError("Server worker EOF. Closing.").WriteToLog(log.DefaultLogger())
				} else {
					newError("Server worker failed to handle frame: ", err).WriteToLog(log.DefaultLogger())
				}
				return
			}
		}
	}
}

// negotiateVersion handles the Mux.Pro version negotiation process for the server.
// It reads the client's NegotiateVersion frame and returns their supported versions.
func (w *ServerWorker) negotiateVersion(ctx context.Context, reader *buf.BufferedReader) ([]uint32, error) {
	// Set a timeout for version negotiation to prevent hanging connections
	negotiationCtx, cancel := context.WithTimeout(ctx, 5*time.Second) // 5 seconds timeout
	defer cancel()

	readDone := make(chan struct{})
	var clientVersions []uint32
	var negotiationErr error

	go func() {
		defer close(readDone)
		_, clientVersions, negotiationErr = ReadNegotiateVersionFrame(reader)
	}()

	select {
	case <-negotiationCtx.Done():
		return nil, newError("Mux.Pro version negotiation timed out or cancelled.").Base(negotiationCtx.Err())
	case <-readDone:
		if negotiationErr != nil {
			return nil, newError("failed to read client NegotiateVersion frame").Base(negotiationErr)
		}
		return clientVersions, nil
	}
}
