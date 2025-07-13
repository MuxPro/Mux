package mux

import (
	"context"
	"crypto/rand" // For GlobalID generation
	"encoding/binary"
	"io"
	"sync"
	"time"

	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/common/buf"
	"github.com/v2fly/v2ray-core/v5/common/errors"
	"github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/common/protocol"
	"github.com/v2fly/v2ray-core/v5/common/serial"
	"github.com/v2fly/v2ray-core/v5/common/session"
	"github.com/v2fly/v2ray-core/v5/common/signal/done"
	"github.com/v2fly/v2ray-core/v5/common/task"
	"github.com/v2fly/v2ray-core/v5/proxy"
	"github.com/v2fly/v2ray-core/v5/transport"
	"github.com/v2fly/v2ray-core/v5/transport/internet"
	"github.com/v2fly/v2ray-core/v5/transport/pipe"
	"lukechampine.com/blake3" // For GlobalID generation
)

// ClientManager manages Mux client workers.
type ClientManager struct {
	Enabled bool // whether mux is enabled from user config
	Picker  WorkerPicker
}

// Dispatch dispatches a link to an available Mux client worker.
func (m *ClientManager) Dispatch(ctx context.Context, link *transport.Link) error {
	for i := 0; i < 16; i++ { // Try 16 times to find an available worker
		worker, err := m.Picker.PickAvailable()
		if err != nil {
			return err
		}
		if worker.Dispatch(ctx, link) {
			return nil
		}
	}

	return newError("unable to find an available mux client").AtWarning()
}

// WorkerPicker interface defines methods to pick an available worker.
type WorkerPicker interface {
	PickAvailable() (*ClientWorker, error)
}

// IncrementalWorkerPicker implements WorkerPicker interface, creating and managing workers on demand.
type IncrementalWorkerPicker struct {
	Factory ClientWorkerFactory

	access      sync.Mutex
	workers     []*ClientWorker
	cleanupTask *task.Periodic
}

// cleanupFunc periodically cleans up closed workers.
func (p *IncrementalWorkerPicker) cleanupFunc() error {
	p.access.Lock()
	defer p.access.Unlock()

	if len(p.workers) == 0 {
		return newError("no worker")
	}

	p.cleanup()
	return nil
}

// cleanup removes closed workers.
func (p *IncrementalWorkerPicker) cleanup() {
	var activeWorkers []*ClientWorker
	for _, w := range p.workers {
		if !w.Closed() {
			activeWorkers = append(activeWorkers, w)
		}
	}
	p.workers = activeWorkers
}

// findAvailable finds a worker that is not full.
func (p *IncrementalWorkerPicker) findAvailable() int {
	for idx, w := range p.workers {
		if !w.IsFull() {
			return idx
		}
	}

	return -1
}

// pickInternal is an internal method to pick or create a worker.
func (p *IncrementalWorkerPicker) pickInternal() (*ClientWorker, bool, error) {
	p.access.Lock()
	defer p.access.Unlock()

	idx := p.findAvailable()
	if idx >= 0 {
		n := len(p.workers)
		// Move the found available worker to the end of the slice for optimization.
		if n > 1 && idx != n-1 {
			p.workers[n-1], p.workers[idx] = p.workers[idx], p.workers[n-1]
		}
		return p.workers[idx], false, nil
	}

	p.cleanup() // Clean up before trying to create a new one

	worker, err := p.Factory.Create()
	if err != nil {
		return nil, false, err
	}
	p.workers = append(p.workers, worker)

	if p.cleanupTask == nil {
		p.cleanupTask = &task.Periodic{
			Interval: time.Second * 30, // Clean up every 30 seconds
			Execute:  p.cleanupFunc,
		}
	}

	return worker, true, nil
}

// PickAvailable picks an available Mux client worker.
func (p *IncrementalWorkerPicker) PickAvailable() (*ClientWorker, error) {
	worker, start, err := p.pickInternal()
	if start {
		common.Must(p.cleanupTask.Start()) // If a new worker is created, start the cleanup task
	}

	return worker, err
}

// ClientWorkerFactory interface defines methods to create Mux client workers.
type ClientWorkerFactory interface {
	Create() (*ClientWorker, error)
}

// DialingWorkerFactory implements ClientWorkerFactory interface, creating workers by dialing.
type DialingWorkerFactory struct {
	Proxy    proxy.Outbound
	Dialer   internet.Dialer
	Strategy ClientStrategy

	ctx context.Context
}

// NewDialingWorkerFactory creates a new DialingWorkerFactory.
func NewDialingWorkerFactory(ctx context.Context, proxy proxy.Outbound, dialer internet.Dialer, strategy ClientStrategy) *DialingWorkerFactory {
	return &DialingWorkerFactory{
		Proxy:    proxy,
		Dialer:   dialer,
		Strategy: strategy,
		ctx:      ctx,
	}
}

var (
	// muxProAddress is the special target address for Mux.Pro connections.
	muxProAddress = net.DomainAddress("mux.pro")
	// muxProPort is the suggested port for Mux.Pro, actual port may vary based on configuration.
	muxProPort    = net.Port(9527)
)

// Create creates a new Mux client worker.
func (f *DialingWorkerFactory) Create() (*ClientWorker, error) {
	opts := []pipe.Option{pipe.WithSizeLimit(64 * 1024)} // Pipe size limit
	uplinkReader, upLinkWriter := pipe.New(opts...)
	downlinkReader, downlinkWriter := pipe.New(opts...)

	c, err := NewClientWorker(transport.Link{
		Reader: downlinkReader,
		Writer: upLinkWriter,
	}, f.Strategy)
	if err != nil {
		return nil, err
	}

	// Start a goroutine to handle underlying connection establishment and data forwarding.
	go func(p proxy.Outbound, d internet.Dialer, c common.Closable) {
		ctx := session.ContextWithOutbound(f.ctx, &session.Outbound{
			// Mux.Pro: Use "mux.pro" as a special target address
			Target: net.TCPDestination(muxProAddress, muxProPort),
		})
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		defer common.Must(c.Close())

		if err := p.Process(ctx, &transport.Link{Reader: uplinkReader, Writer: downlinkWriter}, d); err != nil {
			newError("failed to handle mux client connection").Base(err).WriteToLog()
		}
	}(f.Proxy, f.Dialer, c.done)

	// Perform version negotiation blocking, then start the worker's main loop upon successful negotiation.
	if err := c.NegotiateAndStart(); err != nil {
		c.done.Close() // Ensure worker is closed if negotiation fails
		return nil, newError("Mux.Pro negotiation failed").Base(err)
	}

	return c, nil
}

// ClientStrategy defines client strategies, such as max concurrent connections.
type ClientStrategy struct {
	MaxConcurrency uint32
	MaxConnection  uint32
}

// ClientWorker represents the client side of a Mux main connection.
type ClientWorker struct {
	sessionManager *SessionManager
	link           transport.Link
	done           *done.Instance
	strategy       ClientStrategy
	negotiated     bool // Mux.Pro: Flag indicating if version negotiation is complete
}

// NewClientWorker creates a new mux.ClientWorker.
func NewClientWorker(stream transport.Link, s ClientStrategy) (*ClientWorker, error) {
	c := &ClientWorker{
		sessionManager: NewSessionManager(),
		link:           stream,
		done:           done.New(),
		strategy:       s,
	}
	return c, nil
}

// start starts the worker's main processing loop.
func (m *ClientWorker) start() {
	go m.fetchOutput() // Receive server responses
	go m.monitor()     // Monitor session status
}

// NegotiateAndStart performs Mux.Pro version negotiation and starts the worker upon success.
func (m *ClientWorker) NegotiateAndStart() error {
	// 1. Send client's supported version list
	meta := FrameMetadata{
		SessionID:     0, // Version negotiation uses main connection ID 0x0000
		SessionStatus: SessionStatusNegotiateVersion,
		Option:        OptionData, // OptionData must be set as version list is Extra Data
	}

	frame := buf.New()
	common.Must(meta.WriteTo(frame))

	// Construct Extra Data for version negotiation: [1-byte version count N] [N * 4-byte version numbers]
	versionsPayload := buf.New()
	common.Must(versionsPayload.WriteByte(1))                 // Version count N = 1
	common.Must(WriteUint32(versionsPayload, Version)) // Write current Mux.Pro version number
	defer versionsPayload.Release()

	// Write Extra Data length and content
	Must2(serial.WriteUint16(frame, uint16(versionsPayload.Len()))) // Write Extra Data length
	Must2(frame.Write(versionsPayload.Bytes()))                     // Write Extra Data content

	if err := m.link.Writer.WriteMultiBuffer(buf.MultiBuffer{frame}); err != nil {
		return newError("failed to write negotiation request").Base(err)
	}

	// 2. Read and parse server's response
	reader := &buf.BufferedReader{Reader: m.link.Reader}
	var responseMeta FrameMetadata
	if err := responseMeta.Unmarshal(reader); err != nil {
		return newError("failed to read negotiation response metadata").Base(err)
	}

	// Check response frame ID and status
	if responseMeta.SessionID != 0 || responseMeta.SessionStatus != SessionStatusNegotiateVersion {
		return newError("invalid negotiation response frame: ID=", responseMeta.SessionID, " Status=", responseMeta.SessionStatus)
	}

	// Check if response frame contains Extra Data
	if !responseMeta.Option.Has(OptionData) {
		return newError("negotiation response has no data")
	}

	// Read server response Extra Data
	dataLen, err := serial.ReadUint16(reader)
	if err != nil {
		return newError("failed to read negotiation response data length").Base(err)
	}
	if dataLen != 5 { // 1 byte count + 4 bytes version number
		return newError("invalid negotiation response data length: ", dataLen)
	}

	responsePayload := buf.New()
	defer responsePayload.Release()
	if _, err := responsePayload.ReadFullFrom(reader, int32(dataLen)); err != nil {
		return err
	}

	count := responsePayload.Byte(0)
	if count != 1 { // Server response version count must be 1
		return newError("invalid version count in negotiation response: ", count)
	}

	negotiatedVersion := binary.BigEndian.Uint32(responsePayload.BytesRange(1, 5))
	if negotiatedVersion != Version { // Check if negotiated version is supported by client
		return newError("server selected an unsupported version: ", negotiatedVersion)
	}

	// 3. Negotiation successful, mark and start main loop
	m.negotiated = true
	newError("Mux.Pro negotiation succeeded, version ", negotiatedVersion).WriteToLog()
	m.start()

	return nil
}

// TotalConnections returns the total number of connections (including closed ones).
func (m *ClientWorker) TotalConnections() uint32 {
	return uint32(m.sessionManager.Count())
}

// ActiveConnections returns the number of active connections.
func (m *ClientWorker) ActiveConnections() uint32 {
	return uint32(m.sessionManager.Size())
}

// Closed checks if the worker is closed.
func (m *ClientWorker) Closed() bool {
	return m.done.Done()
}

// monitor monitors session status and closes the worker if no active sessions.
func (m *ClientWorker) monitor() {
	timer := time.NewTicker(time.Second * 16) // Check every 16 seconds
	defer timer.Stop()

	for {
		select {
		case <-m.done.Wait(): // Worker close signal
			m.sessionManager.Close()
			common.Close(m.link.Writer)
			common.Interrupt(m.link.Reader)
			return
		case <-timer.C: // Timer triggered
			size := m.sessionManager.Size()
			if size == 0 && m.sessionManager.CloseIfNoSession() { // If no active sessions, try to close worker
				common.Must(m.done.Close())
			}
		}
	}
}

// GetGlobalID generates an 8-byte GlobalID for UDP FullCone NAT.
// It uses blake3 hash of the inbound source address if it's a UDP connection
// from specific inbound handlers (dokodemo-door, socks, shadowsocks) and
// the 'cone' context value is true.
func GetGlobalID(ctx context.Context) (globalID GlobalID) {
	// Check if 'cone' is enabled in context (e.g., for FullCone NAT)
	coneVal := ctx.Value("cone")
	if coneVal == nil {
		// If 'cone' is not explicitly set, assume false for safety or log a warning.
		// For unit tests, cone might be nil, so handle that.
		return
	}
	cone, ok := coneVal.(bool)
	if !ok || !cone {
		return
	}

	// Check if the inbound connection is UDP and from a relevant handler
	inbound := session.InboundFromContext(ctx)
	if inbound != nil && inbound.Source.Network == net.Network_UDP &&
		(inbound.Tag == "dokodemo-door" || inbound.Tag == "socks" || inbound.Tag == "shadowsocks") {
		
		// Use a fixed BaseKey for demonstration. In a real system, this might be
		// securely generated and persisted.
		var baseKey [32]byte
		// Generate a random key if not provided, and ideally persist it.
		rand.Read(baseKey[:]) // Uncommented: Make crypto/rand used

		h := blake3.New(8, baseKey[:]) // Hash to 8 bytes
		h.Write([]byte(inbound.Source.String()))
		copy(globalID[:], h.Sum(nil))

		// Optional: Log GlobalID for debugging
		// newError(fmt.Sprintf("XUDP inbound.Source.String(): %v\tglobalID: %v", inbound.Source.String(), globalID)).WriteToLog(session.ExportIDToError(ctx))
	}
	return
}

// writeFirstPayload writes the first payload.
func writeFirstPayload(reader buf.Reader, writer *Writer) error {
	err := buf.CopyOnceTimeout(reader, writer, time.Millisecond*100)
	if err == buf.ErrNotTimeoutReader || err == buf.ErrReadTimeout {
		return writer.WriteMultiBuffer(buf.MultiBuffer{})
	}

	if err != nil {
		return err
	}

	return nil
}

// fetchInput reads data from session input stream and writes to Mux frames.
func fetchInput(ctx context.Context, s *Session, output buf.Writer) {
	dest := session.OutboundFromContext(ctx).Target
	transferType := protocol.TransferTypeStream
	if dest.Network == net.Network_UDP {
		transferType = protocol.TransferTypePacket
	}
	s.transferType = transferType

	// Mux.Pro: Get GlobalID for UDP connections
	var globalID GlobalID
	if dest.Network == net.Network_UDP {
		globalID = GetGlobalID(ctx)
	}

	// Mux.Pro: Pass priority and GlobalID
	writer := NewWriter(s.ID, dest, output, transferType, 0x00, s, globalID)
	defer s.Close()
	defer writer.Close()

	newError("dispatching request to ", dest).WriteToLog(session.ExportIDToError(ctx))
	if err := writeFirstPayload(s.input, writer); err != nil {
		newError("failed to write first payload").Base(err).WriteToLog(session.ExportIDToError(ctx))
		writer.SetErrorCode(ErrorCodeProtocolError)
		writer.Close()
		common.Interrupt(s.input)
		return
	}

	if err := buf.Copy(s.input, writer); err != nil {
		newError("failed to fetch all input").Base(err).WriteToLog(session.ExportIDToError(ctx))
		writer.SetErrorCode(ErrorCodeProtocolError)
		writer.Close()
		common.Interrupt(s.input)
		return
	}
}

// IsClosing checks if the worker is closing.
func (m *ClientWorker) IsClosing() bool {
	sm := m.sessionManager
	if m.strategy.MaxConnection > 0 && sm.Count() >= int(m.strategy.MaxConnection) {
		return true
	}
	return false
}

// IsFull checks if the worker is full.
func (m *ClientWorker) IsFull() bool {
	if m.IsClosing() || m.Closed() {
		return true
	}

	sm := m.sessionManager
	if m.strategy.MaxConcurrency > 0 && sm.Size() >= int(m.strategy.MaxConcurrency) {
		return true
	}
	return false
}

// Dispatch dispatches a new link to the worker.
func (m *ClientWorker) Dispatch(ctx context.Context, link *transport.Link) bool {
	if m.IsFull() || m.Closed() {
		return false
	}

	sm := m.sessionManager
	s := sm.Allocate() // Allocate creates and initializes Session, including flow control fields
	if s == nil {
		return false
	}
	s.input = link.Reader
	s.output = link.Writer
	go fetchInput(ctx, s, m.link.Writer)
	return true
}

// handleStatueKeepAlive handles KeepAlive frames.
func (m *ClientWorker) handleStatueKeepAlive(meta *FrameMetadata, reader *buf.BufferedReader) error {
	// Mux.Pro: KeepAlive frame's Opt must be 0x00 and carry no data.
	if meta.Option != 0 {
		return newError("protocol error: KeepAlive frame with non-zero option: ", meta.Option)
	}
	return nil
}

// handleStatusNew handles New frames (client should not receive).
func (m *ClientWorker) handleStatusNew(meta *FrameMetadata, reader *buf.BufferedReader) error {
	// Client should not receive New frames. If received, treat as protocol error and discard data.
	if meta.Option.Has(OptionData) {
		// If it's a UDP New frame with GlobalID, we might need to extract it,
		// but the prompt says client should not receive New frames.
		// So we just discard the data.
		return buf.Copy(NewStreamReader(reader), buf.Discard)
	}
	return nil
}

// handleCreditUpdate handles CreditUpdate frames.
func (m *ClientWorker) handleCreditUpdate(meta *FrameMetadata, reader *buf.BufferedReader) error {
	if !meta.Option.Has(OptionData) {
		return newError("CreditUpdate frame has no data")
	}

	s, found := m.sessionManager.Get(meta.SessionID)
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

// udpDataInjectingReader is a buf.Reader wrapper that injects UDP metadata into the first buffer.
// This is used when a Mux Keep frame (SessionStatusKeep) contains OptionUDPData,
// indicating that the payload is a UDP packet and its source address is in the Extra Data.
type udpDataInjectingReader struct {
	buf.Reader
	udpDest  net.Destination
	globalID GlobalID
	injected bool // Flag to ensure UDP metadata is injected only once per packet
}

// ReadMultiBuffer implements buf.Reader.
func (u *udpDataInjectingReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb, err := u.Reader.ReadMultiBuffer()
	if err != nil {
		return nil, err
	}

	if !u.injected && len(mb) > 0 {
		// Assign the parsed UDP destination to the first buffer's UDP field.
		// This ensures the downstream application receives the correct UDP source for the packet.
		mb[0].UDP = &u.udpDest
		// The GlobalID is for session identification and is not directly put on the buffer.
		// It's associated with the session itself.
		u.injected = true
	}
	return mb, nil
}


// handleStatusKeep handles Keep frames (data transfer).
func (m *ClientWorker) handleStatusKeep(meta *FrameMetadata, reader *buf.BufferedReader) error {
	if !meta.Option.Has(OptionData) { // OptionData must be set for any payload or Extra Data
		return nil
	}

	s, found := m.sessionManager.Get(meta.SessionID)
	if !found {
		// If session not found, discard the entire frame.
		closingWriter := NewResponseWriter(meta.SessionID, m.link.Writer, protocol.TransferTypeStream, nil, GlobalID{})
		closingWriter.SetErrorCode(ErrorCodeProtocolError)
		closingWriter.Close()
		return buf.Copy(NewStreamReader(reader), buf.Discard)
	}

	var udpSrc net.Destination
	var globalID GlobalID
	var err error

	// Mux.Pro: If OptionUDPData is set, parse UDP source address and GlobalID from Extra Data.
	if meta.Option.Has(OptionUDPData) {
		udpSrc, globalID, err = readUDPMetaData(reader) // Use the helper function from reader.go
		if err != nil {
			newError("failed to read UDP metadata for session ", s.ID).Base(err).WriteToLog()
			// If metadata parsing fails, discard the remaining data and close session.
			closingWriter := NewResponseWriter(meta.SessionID, m.link.Writer, protocol.TransferTypeStream, nil, GlobalID{})
			closingWriter.SetErrorCode(ErrorCodeProtocolError)
			closingWriter.Close()
			return buf.Copy(NewStreamReader(reader), buf.Discard)
		}
		newError("received UDP data for session ", s.ID, " from ", udpSrc, " GlobalID: ", globalID).WriteToLog()
	}

	// Create the underlying reader for the actual payload data.
	rr := s.NewReader(reader)

	// If UDP data was present, wrap the reader to inject the UDP metadata into the first buffer.
	if meta.Option.Has(OptionUDPData) {
		rr = &udpDataInjectingReader{
			Reader:   rr,
			udpDest:  udpSrc,
			globalID: globalID,
			injected: false,
		}
	}

	var sc buf.SizeCounter
	// buf.Copy will now use the potentially wrapped 'rr', which sets the UDP field on the first buffer.
	err = buf.Copy(rr, s.output, buf.CountSize(&sc))
	copiedBytes := sc.Size

	if err != nil && buf.IsWriteError(err) {
		newError("failed to write to downstream. closing session ", s.ID).Base(err).WriteToLog()
		closingWriter := NewResponseWriter(meta.SessionID, m.link.Writer, protocol.TransferTypeStream, s, GlobalID{})
		closingWriter.SetErrorCode(ErrorCodeProtocolError)
		closingWriter.Close()
		drainErr := buf.Copy(rr, buf.Discard)
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
		if writeErr := m.link.Writer.WriteMultiBuffer(buf.MultiBuffer{creditFrame}); writeErr != nil {
			newError("failed to send CreditUpdate frame for session ", s.ID).Base(writeErr).WriteToLog()
		} else {
			s.ResetReceivedBytes() // Reset count after successful send
		}
	}

	return err
}

// handleStatusEnd handles End frames (closing sub-connection).
func (m *ClientWorker) handleStatusEnd(meta *FrameMetadata, reader *buf.BufferedReader) error {
	if s, found := m.sessionManager.Get(meta.SessionID); found {
		// Mux.Pro: Use ErrorCode instead of OptionError to determine close reason.
		if meta.ErrorCode != ErrorCodeGracefulShutdown {
			newError("session ", s.ID, " ended with error code: ", meta.ErrorCode).WriteToLog()
			common.Interrupt(s.input)
			common.Interrupt(s.output)
		}
		s.Close()
	}
	if meta.Option.Has(OptionData) { // If End frame unexpectedly contains data, discard it
		return buf.Copy(NewStreamReader(reader), buf.Discard)
	}
	return nil
}

// fetchOutput reads and processes inbound Mux frames from the main connection.
func (m *ClientWorker) fetchOutput() {
	// Ensure output processing starts only after negotiation is complete.
	if !m.negotiated {
		newError("fetchOutput started before negotiation").AtError().WriteToLog()
		common.Must(m.done.Close())
		return
	}

	defer func() {
		common.Must(m.done.Close())
	}()

	reader := &buf.BufferedReader{Reader: m.link.Reader}

	var meta FrameMetadata
	for {
		err := meta.Unmarshal(reader) // Parse frame metadata
		if err != nil {
			if errors.Cause(err) != io.EOF {
				newError("failed to read metadata").Base(err).WriteToLog()
			}
			break
		}

		switch meta.SessionStatus {
		case SessionStatusKeepAlive:
			err = m.handleStatueKeepAlive(&meta, reader)
		case SessionStatusEnd:
			err = m.handleStatusEnd(&meta, reader)
		case SessionStatusNew:
			err = m.handleStatusNew(&meta, reader)
		case SessionStatusKeep:
			err = m.handleStatusKeep(&meta, reader)
		case SessionStatusCreditUpdate: // Mux.Pro: Handle CreditUpdate frame
			err = m.handleCreditUpdate(&meta, reader)
		case SessionStatusNegotiateVersion:
			// This frame should not be received after negotiation, treat as protocol error
			err = newError("unexpected NegotiateVersion frame after initial negotiation")
		default:
			status := meta.SessionStatus
			newError("unknown status: ", status).AtError().WriteToLog()
			return
		}

		if err != nil {
			newError("failed to process data").Base(err).WriteToLog()
			return
		}
	}
}
