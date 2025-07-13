package mux

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/common/buf"
	"github.com/v2fly/v2ray-core/v5/common/errors"
	"github.com/v2fly/v2ray-core/v5/common/log"
	"github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/common/protocol"
	"github.com/v2fly/v2ray-core/v5/common/session"
	"github.com/v2fly/v2ray-core/v5/common/signal/done"
	"github.com/v2fly/v2ray-core/v5/common/task"
	"github.com/v2fly/v2ray-core/v5/proxy"
	"github.com/v2fly/v2ray-core/v5/transport"
	"github.com/v2fly/v2ray-core/v5/transport/internet"
	"github.com/v2fly/v2ray-core/v5/transport/pipe"
)

type ClientManager struct {
	Enabled bool // whether mux is enabled from user config
	Picker  WorkerPicker
}

func (m *ClientManager) Dispatch(ctx context.Context, link *transport.Link) error {
	for i := 0; i < 16; i++ { // Try to pick an existing worker
		worker, err := m.Picker.PickAvailable()
		if err != nil {
			if errors.Is(err, ErrNoWorker) {
				break
			}
			return newError("failed to pick worker").Base(err)
		}
		if err := worker.Dispatch(ctx, link); err == nil {
			return nil
		}
	}

	worker, err := m.Picker.PickOrCreate() // Create a new worker if no available
	if err != nil {
		return newError("failed to pick or create worker").Base(err)
	}
	return worker.Dispatch(ctx, link)
}

type WorkerPicker interface {
	PickAvailable() (ClientWorker, error)
	PickOrCreate() (ClientWorker, error)
	Release(ClientWorker)
}

type ClientWorker interface {
	Dispatch(context.Context, *transport.Link) error
	IsFull() bool
	IsClosing() bool
}

type ClientWorkerFactory interface {
	Create() (ClientWorker, error)
}

type IncrementalWorkerPicker struct {
	sync.RWMutex
	workers []ClientWorker
	factory ClientWorkerFactory
	strategy ClientStrategy
}

func NewIncrementalWorkerPicker(f ClientWorkerFactory, s ClientStrategy) *IncrementalWorkerPicker {
	return &IncrementalWorkerPicker{
		workers: make([]ClientWorker, 0, 4),
		factory: f,
		strategy: s,
	}
}

func (p *IncrementalWorkerPicker) PickAvailable() (ClientWorker, error) {
	p.RLock()
	defer p.RUnlock()

	if len(p.workers) == 0 {
		return nil, ErrNoWorker
	}

	for _, worker := range p.workers {
		if !worker.IsFull() && !worker.IsClosing() {
			return worker, nil
		}
	}
	return nil, ErrNoWorker
}

func (p *IncrementalWorkerPicker) PickOrCreate() (ClientWorker, error) {
	p.Lock()
	defer p.Unlock()

	for _, worker := range p.workers {
		if !worker.IsFull() && !worker.IsClosing() {
			return worker, nil
		}
	}

	if len(p.workers) >= p.strategy.MaxConnection {
		return nil, newError("max number of connections reached").AtError()
	}

	worker, err := p.factory.Create()
	if err != nil {
		return nil, newError("failed to create worker").Base(err)
	}
	p.workers = append(p.workers, worker)
	return worker, nil
}

func (p *IncrementalWorkerPicker) Release(w ClientWorker) {
	p.Lock()
	defer p.Unlock()

	for idx, worker := range p.workers {
		if worker == w {
			p.workers = append(p.workers[:idx], p.workers[idx+1:]...)
			return
		}
	}
}

type DialingWorkerFactory struct {
	Proxy proxy.Outbound
}

func (f *DialingWorkerFactory) Create() (ClientWorker, error) {
	// Mux.Pro: Target address and port change
	dest := net.Destination{
		Address: muxProAddress,
		Port:    muxProPort,
		Network: net.Network_TCP, // Mux.Pro operates over reliable streams, usually TCP
	}

	ctx, cancel := context.WithCancel(context.Background())
	link, err := f.Proxy.Process(ctx, transport.With =session.NewOutbound(dest)), internet.Dial(ctx, dest)
	if err != nil {
		cancel()
		return nil, newError("failed to dial to ", dest).Base(err)
	}

	worker := NewClientWorker(link, ClientStrategy{MaxConcurrency: 8, MaxConnection: 1}) // Example strategy
	go func() {
		defer cancel()
		<-worker.(*clientWorker).done.Done() // Wait for worker to close
	}()

	return worker, nil
}

type ClientStrategy struct {
	MaxConcurrency int // Number of concurrent streams on a single Mux connection
	MaxConnection  int // Number of Mux connections per outbound
}

type clientWorker struct {
	sessionManager *SessionManager
	link           *transport.Link
	done           *done.Done
	strategy       ClientStrategy
	priorityMtx    sync.Mutex // For managing priority allocation
	nextPriority   byte       // Mux.Pro: Next priority to assign, for round-robin or similar
}

func NewClientWorker(stream transport.Link, s ClientStrategy) ClientWorker {
	worker := &clientWorker{
		sessionManager: NewSessionManager(),
		link:           stream,
		done:           done.New(),
		strategy:       s,
		nextPriority:   0x00, // Mux.Pro: Start with highest priority
	}

	go worker.monitor()
	go worker.fetchOutput()
	go worker.sendKeepAlive() // Mux.Pro: Periodically send KeepAlive

	return worker
}

// monitor closes the worker if there are no more active sessions.
func (m *clientWorker) monitor() {
	defer m.done.Done()
	<-time.After(30 * time.Second) // Wait for some initial activity

	for {
		if m.sessionManager.Closed() {
			newError("ClientWorker: Session manager closed. Closing worker.").WriteToLog(log.DefaultLogger())
			return
		}

		if m.sessionManager.Size() == 0 {
			if m.sessionManager.CloseIfNoSession() {
				newError("ClientWorker: No active sessions. Closing worker.").WriteToLog(log.DefaultLogger())
				return
			}
		}
		time.Sleep(5 * time.Second) // Check every 5 seconds
	}
}

// sendKeepAlive periodically sends Mux.Pro KeepAlive frames.
func (m *clientWorker) sendKeepAlive() {
	ticker := time.NewTicker(30 * time.Second) // Mux.Pro: Send KeepAlive every 30 seconds
	defer ticker.Stop()

	for {
		select {
		case <-m.done.Done():
			return
		case <-ticker.C:
			if !m.sessionManager.Closed() {
				err := WriteKeepAliveFrame(m.link.Writer) // Mux.Pro: Use new WriteKeepAliveFrame
				if err != nil {
					newError("Client: Failed to send KeepAlive frame: ", err).WriteToLog(log.DefaultLogger())
					// On error, consider closing the worker if it's critical.
				}
			}
		}
	}
}

func (m *clientWorker) writeFirstPayload(reader buf.Reader, writer *Writer) error {
	mb, err := reader.ReadMultiBufferTimeout(500 * time.Millisecond) // Read initial data with timeout
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, io.EOF) {
		return err
	}

	if mb == nil || mb.IsEmpty() {
		return writer.writeMetaOnly() // Send New frame without data
	}

	return writer.WriteMultiBuffer(mb) // Send New frame with data
}

func (m *clientWorker) fetchInput(ctx context.Context, s *Session, output buf.Writer) {
	defer common.Close(s.input)
	defer common.Close(output)
	defer s.Close()

	// Mux.Pro: Credit-based flow control for sending data to server
	// We need to wait for credit from the server before sending data.
	// For simplicity, we assume an initial default credit on session creation,
	// and then wait for CreditUpdate frames from the server.
	// A more robust implementation would buffer data if credit is insufficient.

	for {
		select {
		case <-ctx.Done():
			return
		default:
			if s.GetCredit() == 0 {
				time.Sleep(10 * time.Millisecond) // Wait for credit
				continue
			}

			// Read from s.input (application data)
			mb, err := s.input.ReadMultiBuffer()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					newError("session ", s.ID, " read from input ended with error").Base(err).WriteToLog(log.DefaultLogger())
					// Mux.Pro: Send End frame with appropriate error code
					common.Close(output.Close(ErrorCodeRemoteDisconnect))
				} else {
					common.Close(output.Close(ErrorCodeGracefulShutdown))
				}
				return
			}

			if mb.IsEmpty() {
				continue
			}

			// Mux.Pro: Consume credit for outgoing data
			dataLen := uint32(mb.Len())
			if !s.ConsumeCredit(dataLen) {
				newError("session ", s.ID, " attempting to send data without enough credit. Available: ", s.GetCredit(), ", Needed: ", dataLen).WriteToLog(log.DefaultLogger())
				buf.ReleaseMulti(mb) // Release data as we can't send it
				// This indicates a flow control issue.
				// Could buffer data or close session. For now, close session.
				common.Close(output.Close(ErrorCodeProtocolError))
				return
			}

			if err := output.WriteMultiBuffer(mb); err != nil {
				if !errors.Is(err, io.EOF) {
					newError("session ", s.ID, " write to output ended with error").Base(err).WriteToLog(log.DefaultLogger())
					// Mux.Pro: Send End frame with appropriate error code
					common.Close(output.Close(ErrorCodeRemoteDisconnect))
				}
				return
			}
		}
	}
}

func (m *clientWorker) IsClosing() bool {
	return m.done.Done() != nil // True if the channel is closed
}

func (m *clientWorker) IsFull() bool {
	return m.sessionManager.Size() >= m.strategy.MaxConcurrency
}

func (m *clientWorker) Dispatch(ctx context.Context, link *transport.Link) error {
	m.priorityMtx.Lock()
	priority := m.nextPriority
	m.nextPriority = (m.nextPriority + 1) % 0x100 // Cycle through 0x00 to 0xFF
	m.priorityMtx.Unlock()

	s := m.sessionManager.Allocate()
	if s == nil {
		return newError("failed to allocate session").AtError()
	}
	s.input = link.Reader
	s.output = link.Writer
	s.transferType = protocol.TransferTypeStream // Assuming stream for most V2Ray connections

	// Mux.Pro: Client sends New frame with priority
	newError("Client: Dispatching new session ", s.ID, " with priority ", priority).WriteToLog(log.DefaultLogger())
	writer := NewWriter(s.ID, link.Target, m.link.Writer, s.transferType, priority) // Pass priority

	go m.fetchInput(ctx, s, writer)

	return nil
}

func (m *clientWorker) handleStatueKeepAlive(meta *FrameMetadata, reader *buf.BufferedReader) error {
	// Mux.Pro: Opt MUST be 0x00. No Extra Data.
	if meta.Option != 0x00 {
		return newError("Mux.Pro KeepAlive Opt must be 0x00").AtError()
	}
	// As per Mux.Pro, KeepAlive MUST NOT carry any Extra Data.
	if meta.Option.Has(OptionData) {
		return newError("Mux.Pro KeepAlive must not have Extra Data").AtError()
	}
	return nil
}

// handleStatusNew is technically not expected on the client side according to Mux.Pro spec,
// but for robustness, it should discard any such unexpected frames.
func (m *clientWorker) handleStatusNew(meta *FrameMetadata, reader *buf.BufferedReader) error {
	newError("Client: Received unexpected New frame for session ", meta.SessionID, ". Discarding.").WriteToLog(log.DefaultLogger())
	if meta.Option.Has(OptionData) {
		return buf.Copy(NewStreamReader(reader), buf.Discard)
	}
	return nil
}

func (m *clientWorker) handleStatusKeep(meta *FrameMetadata, reader *buf.BufferedReader) error {
	if !meta.Option.Has(OptionData) {
		return nil // No data to process
	}

	s, found := m.sessionManager.Get(meta.SessionID)
	if !found {
		newError("Client: Received data for unknown session ", meta.SessionID, ". Discarding.").WriteToLog(log.DefaultLogger())
		// Mux.Pro: Client should also send End frame if it gets data for unknown session
		closingWriter := NewResponseWriter(meta.SessionID, m.link.Writer, protocol.TransferTypeStream)
		common.Close(closingWriter.Close(ErrorCodeProtocolError))
		return buf.Copy(NewStreamReader(reader), buf.Discard)
	}

	// Mux.Pro: Flow control: Server sending data means we (client) received it.
	// We should grant credit back to the server after processing.
	dataLen := reader.Buffer().Len() // Total length of data in the current frame
	if dataLen > 0 {
		// Example: Grant credit back every time we receive data.
		// A more sophisticated approach would buffer or process, then grant.
		err := WriteCreditUpdateFrame(m.link.Writer, s.ID, uint32(dataLen)) // Grant back what was just received
		if err != nil {
			newError("Client: Failed to send credit update for session ", s.ID).Base(err).WriteToLog(log.DefaultLogger())
			// This is not critical enough to close session immediately, but logs are useful.
		}
	}

	_, err := buf.Copy(NewStreamReader(reader), s.output)
	if err != nil {
		newError("Client: Failed to write data for session ", meta.SessionID).Base(err).WriteToLog(log.DefaultLogger())
		// Mux.Pro: Client received error from application. Send End frame.
		common.Close(NewResponseWriter(s.ID, m.link.Writer, s.transferType).Close(ErrorCodeRemoteDisconnect))
		s.Close() // Close local session too
		return err
	}
	return nil
}

func (m *clientWorker) handleStatusEnd(meta *FrameMetadata, reader *buf.BufferedReader) error {
	s, found := m.sessionManager.Get(meta.SessionID)
	if found {
		// Mux.Pro: Read ErrorCode from metadata
		newError("Client: Session ", meta.SessionID, " closed with error code: ", meta.ErrorCode).WriteToLog(log.DefaultLogger())
		common.Interrupt(s.input)
		common.Interrupt(s.output)
		s.Close()
	}
	// Mux.Pro: Discard any extra data if OptionData was set for End frame (protocol violation)
	if meta.Option.Has(OptionData) {
		return buf.Copy(NewStreamReader(reader), buf.Discard)
	}
	return nil
}

// handleStatusCreditUpdate handles incoming CreditUpdate frames from the server.
func (m *clientWorker) handleStatusCreditUpdate(meta *FrameMetadata, reader *buf.BufferedReader) error {
	_, increment, err := ReadCreditUpdateFrame(reader)
	if err != nil {
		return newError("Client: Failed to read CreditUpdate frame").Base(err).AtError()
	}

	s, found := m.sessionManager.Get(meta.SessionID)
	if !found {
		newError("Client: Received credit update for unknown session ", meta.SessionID, ". Discarding.").WriteToLog(log.DefaultLogger())
		return nil // Just discard for unknown session
	}

	s.AddCredit(increment)
	newError("Client: Session ", s.ID, " credit updated by ", increment, ". New credit: ", s.GetCredit()).WriteToLog(log.DefaultLogger())
	return nil
}

func (m *clientWorker) fetchOutput() {
	defer func() {
		common.Must(m.done.Close()) // Signal worker is done
		newError("ClientWorker: fetchOutput goroutine finished. Releasing worker.").WriteToLog(log.DefaultLogger())
		// Release worker from picker if it was managed
		// This requires picker to be accessible here, or a callback.
		// For now, it will rely on the monitor to close if no sessions.
	}()

	reader := &buf.BufferedReader{Reader: m.link.Reader}

	// Mux.Pro: Version Negotiation (REQUIRED)
	newError("Client: Starting Mux.Pro version negotiation.").WriteToLog(log.DefaultLogger())
	err := m.negotiateVersion(context.Background(), reader) // Use a context for negotiation
	if err != nil {
		newError("Client: Mux.Pro version negotiation failed: ", err).WriteToLog(log.DefaultLogger())
		return // Close connection on negotiation failure
	}
	newError("Client: Mux.Pro version negotiation successful.").WriteToLog(log.DefaultLogger())

	// Main loop for Mux.Pro frame processing
	var meta FrameMetadata
	for {
		if m.sessionManager.Closed() {
			break
		}

		select {
		case <-m.done.Done():
			newError("Client worker context done. Closing.").WriteToLog(log.DefaultLogger())
			return
		default:
			err = meta.Unmarshal(reader)
			if err != nil {
				if errors.Is(err, io.EOF) {
					newError("Client worker EOF. Closing.").WriteToLog(log.DefaultLogger())
				} else {
					newError("Client worker failed to read metadata: ", err).Base(err).WriteToLog(log.DefaultLogger())
				}
				return
			}

			switch meta.SessionStatus {
			case SessionStatusKeepAlive:
				err = m.handleStatueKeepAlive(&meta, reader)
			case SessionStatusEnd:
				err = m.handleStatusEnd(&meta, reader)
			case SessionStatusNew: // Unexpected on client, but handle defensively
				err = m.handleStatusNew(&meta, reader)
			case SessionStatusKeep:
				err = m.handleStatusKeep(&meta, reader)
			case SessionStatusCreditUpdate: // Mux.Pro: New frame type
				err = m.handleStatusCreditUpdate(&meta, reader)
			default:
				status := meta.SessionStatus
				err = newError("unknown status: ", status).AtError()
			}

			if err != nil {
				newError("Client worker failed to process data: ", err).Base(err).WriteToLog(log.DefaultLogger())
				// Attempt to send End frame for the problematic session if possible
				if meta.SessionID != 0x0000 { // If it's a sub-session error
					common.Close(NewResponseWriter(meta.SessionID, m.link.Writer, protocol.TransferTypeStream).Close(ErrorCodeProtocolError))
				}
				return // Critical error, close main connection
			}
		}
	}
}

// negotiateVersion handles the Mux.Pro version negotiation process for the client.
func (m *clientWorker) negotiateVersion(ctx context.Context, reader *buf.BufferedReader) error {
	// Set a timeout for version negotiation
	negotiationCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// 1. Client sends its supported versions (MuxProVersion is the only one we support for now)
	newError("Client: Sending Mux.Pro NegotiateVersion frame with version ", MuxProVersion).WriteToLog(log.DefaultLogger())
	if err := WriteNegotiateVersionFrame(m.link.Writer, []uint32{MuxProVersion}); err != nil {
		return newError("failed to send Mux.Pro NegotiateVersion frame").Base(err)
	}

	// 2. Client reads server's response
	readDone := make(chan struct{})
	var serverMeta FrameMetadata
	var serverVersions []uint32
	var negotiationErr error

	go func() {
		defer close(readDone)
		serverMeta, serverVersions, negotiationErr = ReadNegotiateVersionFrame(reader)
	}()

	select {
	case <-negotiationCtx.Done():
		return newError("Mux.Pro version negotiation timed out or cancelled.").Base(negotiationCtx.Err())
	case <-readDone:
		if negotiationErr != nil {
			return newError("failed to read server NegotiateVersion frame").Base(negotiationErr)
		}
		if len(serverVersions) != 1 || serverVersions[0] != MuxProVersion {
			return newError("server did not negotiate Mux.Pro/0.0, got: ", serverVersions).AtError()
		}
		newError("Client: Mux.Pro version negotiated with server: ", serverVersions[0]).WriteToLog(log.DefaultLogger())
		return nil
	}
}
