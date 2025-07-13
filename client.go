package mux

import (
	"context"
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
)

type ClientManager struct {
	Enabled bool // wheather mux is enabled from user config
	Picker  WorkerPicker
}

func (m *ClientManager) Dispatch(ctx context.Context, link *transport.Link) error {
	for i := 0; i < 16; i++ {
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

type WorkerPicker interface {
	PickAvailable() (*ClientWorker, error)
}

type IncrementalWorkerPicker struct {
	Factory ClientWorkerFactory

	access      sync.Mutex
	workers     []*ClientWorker
	cleanupTask *task.Periodic
}

func (p *IncrementalWorkerPicker) cleanupFunc() error {
	p.access.Lock()
	defer p.access.Unlock()

	if len(p.workers) == 0 {
		return newError("no worker")
	}

	p.cleanup()
	return nil
}

func (p *IncrementalWorkerPicker) cleanup() {
	var activeWorkers []*ClientWorker
	for _, w := range p.workers {
		if !w.Closed() {
			activeWorkers = append(activeWorkers, w)
		}
	}
	p.workers = activeWorkers
}

func (p *IncrementalWorkerPicker) findAvailable() int {
	for idx, w := range p.workers {
		if !w.IsFull() {
			return idx
		}
	}

	return -1
}

func (p *IncrementalWorkerPicker) pickInternal() (*ClientWorker, bool, error) {
	p.access.Lock()
	defer p.access.Unlock()

	idx := p.findAvailable()
	if idx >= 0 {
		n := len(p.workers)
		if n > 1 && idx != n-1 {
			p.workers[n-1], p.workers[idx] = p.workers[idx], p.workers[n-1]
		}
		return p.workers[idx], false, nil
	}

	p.cleanup()

	worker, err := p.Factory.Create()
	if err != nil {
		return nil, false, err
	}
	p.workers = append(p.workers, worker)

	if p.cleanupTask == nil {
		p.cleanupTask = &task.Periodic{
			Interval: time.Second * 30,
			Execute:  p.cleanupFunc,
		}
	}

	return worker, true, nil
}

func (p *IncrementalWorkerPicker) PickAvailable() (*ClientWorker, error) {
	worker, start, err := p.pickInternal()
	if start {
		common.Must(p.cleanupTask.Start())
	}

	return worker, err
}

type ClientWorkerFactory interface {
	Create() (*ClientWorker, error)
}

type DialingWorkerFactory struct {
	Proxy    proxy.Outbound
	Dialer   internet.Dialer
	Strategy ClientStrategy

	ctx context.Context
}

func NewDialingWorkerFactory(ctx context.Context, proxy proxy.Outbound, dialer internet.Dialer, strategy ClientStrategy) *DialingWorkerFactory {
	return &DialingWorkerFactory{
		Proxy:    proxy,
		Dialer:   dialer,
		Strategy: strategy,
		ctx:      ctx,
	}
}

var (
	// muxProAddress 是 Mux.Pro 连接的目标地址。
	muxProAddress = net.DomainAddress("mux.pro")
	muxProPort    = net.Port(9527)
)

func (f *DialingWorkerFactory) Create() (*ClientWorker, error) {
	opts := []pipe.Option{pipe.WithSizeLimit(64 * 1024)}
	uplinkReader, upLinkWriter := pipe.New(opts...)
	downlinkReader, downlinkWriter := pipe.New(opts...)

	c, err := NewClientWorker(transport.Link{
		Reader: downlinkReader,
		Writer: upLinkWriter,
	}, f.Strategy)
	if err != nil {
		return nil, err
	}

	// 启动一个 goroutine 来处理底层连接的建立和数据转发。
	go func(p proxy.Outbound, d internet.Dialer, c common.Closable) {
		ctx := session.ContextWithOutbound(f.ctx, &session.Outbound{
			Target: net.TCPDestination(muxProAddress, muxProPort),
		})
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		defer common.Must(c.Close())

		if err := p.Process(ctx, &transport.Link{Reader: uplinkReader, Writer: downlinkWriter}, d); err != nil {
			newError("failed to handle mux client connection").Base(err).WriteToLog()
		}
	}(f.Proxy, f.Dialer, c.done)

	// 阻塞地执行版本协商，协商成功后启动 worker 的主循环。
	if err := c.NegotiateAndStart(); err != nil {
		c.done.Close() // 确保在协商失败时关闭 worker
		return nil, newError("Mux.Pro negotiation failed").Base(err)
	}

	return c, nil
}

type ClientStrategy struct {
	MaxConcurrency uint32
	MaxConnection  uint32
}

type ClientWorker struct {
	sessionManager *SessionManager
	link           transport.Link
	done           *done.Instance
	strategy       ClientStrategy
	negotiated     bool // Mux.Pro: 标记是否已完成版本协商
}

// NewClientWorker 创建一个新的 mux.ClientWorker。
func NewClientWorker(stream transport.Link, s ClientStrategy) (*ClientWorker, error) {
	c := &ClientWorker{
		sessionManager: NewSessionManager(),
		link:           stream,
		done:           done.New(),
		strategy:       s,
	}
	return c, nil
}

// start 启动 worker 的主处理循环。
func (m *ClientWorker) start() {
	go m.fetchOutput()
	go m.monitor()
}

// NegotiateAndStart 执行 Mux.Pro 版本协商，成功后启动 worker。
func (m *ClientWorker) NegotiateAndStart() error {
	// 1. 发送客户端支持的版本列表
	meta := FrameMetadata{
		SessionID:     0, // 版本协商使用主连接 ID 0
		SessionStatus: SessionStatusNegotiateVersion,
		Option:        OptionData,
	}

	frame := buf.New()
	common.Must(meta.WriteTo(frame))

	// 构造版本协商的 Extra Data
	versionsPayload := buf.New()
	common.Must(versionsPayload.WriteByte(1))                 // 版本数量 N = 1
	common.Must(WriteUint32(versionsPayload, Version)) // 版本号
	defer versionsPayload.Release()

	// 写入 Extra Data 的长度和内容
	Must2(serial.WriteUint16(frame, uint16(versionsPayload.Len()))) // 1. 将长度写入 frame
	Must2(frame.Write(versionsPayload.Bytes()))   

	if err := m.link.Writer.WriteMultiBuffer(buf.MultiBuffer{frame}); err != nil {
		return newError("failed to write negotiation request").Base(err)
	}

	// 2. 读取并解析服务器的响应
	reader := &buf.BufferedReader{Reader: m.link.Reader}
	var responseMeta FrameMetadata
	if err := responseMeta.Unmarshal(reader); err != nil {
		return newError("failed to read negotiation response metadata").Base(err)
	}

	if responseMeta.SessionID != 0 || responseMeta.SessionStatus != SessionStatusNegotiateVersion {
		return newError("invalid negotiation response frame: ID=", responseMeta.SessionID, " Status=", responseMeta.SessionStatus)
	}

	if !responseMeta.Option.Has(OptionData) {
		return newError("negotiation response has no data")
	}

	// 读取服务器响应的 Extra Data
	dataLen, err := serial.ReadUint16(reader)
	if err != nil {
		return newError("failed to read negotiation response data length").Base(err)
	}
	if dataLen != 5 { // 1 字节数量 + 4 字节版本号
		return newError("invalid negotiation response data length: ", dataLen)
	}

	responsePayload := buf.New()
	defer responsePayload.Release()
	if _, err := responsePayload.ReadFullFrom(reader, int32(dataLen)); err != nil {
		return err
	}

	count := responsePayload.Byte(0)
	if count != 1 {
		return newError("invalid version count in negotiation response: ", count)
	}

	negotiatedVersion := binary.BigEndian.Uint32(responsePayload.BytesRange(1, 5))
	if negotiatedVersion != Version {
		return newError("server selected an unsupported version: ", negotiatedVersion)
	}

	// 3. 协商成功，标记并启动主循环
	m.negotiated = true
	newError("Mux.Pro negotiation succeeded, version ", negotiatedVersion).WriteToLog()
	m.start()

	return nil
}

func (m *ClientWorker) TotalConnections() uint32 {
	return uint32(m.sessionManager.Count())
}

func (m *ClientWorker) ActiveConnections() uint32 {
	return uint32(m.sessionManager.Size())
}

func (m *ClientWorker) Closed() bool {
	return m.done.Done()
}

func (m *ClientWorker) monitor() {
	timer := time.NewTicker(time.Second * 16)
	defer timer.Stop()

	for {
		select {
		case <-m.done.Wait():
			m.sessionManager.Close()
			common.Close(m.link.Writer)
			common.Interrupt(m.link.Reader)
			return
		case <-timer.C:
			size := m.sessionManager.Size()
			if size == 0 && m.sessionManager.CloseIfNoSession() {
				common.Must(m.done.Close())
			}
		}
	}
}

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

func fetchInput(ctx context.Context, s *Session, output buf.Writer) {
	dest := session.OutboundFromContext(ctx).Target
	transferType := protocol.TransferTypeStream
	if dest.Network == net.Network_UDP {
		transferType = protocol.TransferTypePacket
	}
	s.transferType = transferType
	writer := NewWriter(s.ID, dest, output, transferType)
	defer s.Close()
	defer writer.Close()

	newError("dispatching request to ", dest).WriteToLog(session.ExportIDToError(ctx))
	if err := writeFirstPayload(s.input, writer); err != nil {
		newError("failed to write first payload").Base(err).WriteToLog(session.ExportIDToError(ctx))
		writer.hasError = true
		common.Interrupt(s.input)
		return
	}

	if err := buf.Copy(s.input, writer); err != nil {
		newError("failed to fetch all input").Base(err).WriteToLog(session.ExportIDToError(ctx))
		writer.hasError = true
		common.Interrupt(s.input)
		return
	}
}

func (m *ClientWorker) IsClosing() bool {
	sm := m.sessionManager
	if m.strategy.MaxConnection > 0 && sm.Count() >= int(m.strategy.MaxConnection) {
		return true
	}
	return false
}

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

func (m *ClientWorker) Dispatch(ctx context.Context, link *transport.Link) bool {
	if m.IsFull() || m.Closed() {
		return false
	}

	sm := m.sessionManager
	s := sm.Allocate()
	if s == nil {
		return false
	}
	s.input = link.Reader
	s.output = link.Writer
	go fetchInput(ctx, s, m.link.Writer)
	return true
}

func (m *ClientWorker) handleStatueKeepAlive(meta *FrameMetadata, reader *buf.BufferedReader) error {
	// Mux.Pro: KeepAlive 帧的 Opt 必须为 0x00，且不带数据。
	if meta.Option != 0 {
		return newError("protocol error: KeepAlive frame with non-zero option: ", meta.Option)
	}
	return nil
}

func (m *ClientWorker) handleStatusNew(meta *FrameMetadata, reader *buf.BufferedReader) error {
	// 客户端不应该收到 New 帧。
	if meta.Option.Has(OptionData) {
		return buf.Copy(NewStreamReader(reader), buf.Discard)
	}
	return nil
}

func (m *ClientWorker) handleStatusKeep(meta *FrameMetadata, reader *buf.BufferedReader) error {
	if !meta.Option.Has(OptionData) {
		return nil
	}

	s, found := m.sessionManager.Get(meta.SessionID)
	if !found {
		closingWriter := NewResponseWriter(meta.SessionID, m.link.Writer, protocol.TransferTypeStream)
		closingWriter.Close()
		return buf.Copy(NewStreamReader(reader), buf.Discard)
	}

	rr := s.NewReader(reader)
	err := buf.Copy(rr, s.output)
	if err != nil && buf.IsWriteError(err) {
		newError("failed to write to downstream. closing session ", s.ID).Base(err).WriteToLog()
		closingWriter := NewResponseWriter(meta.SessionID, m.link.Writer, protocol.TransferTypeStream)
		closingWriter.Close()
		drainErr := buf.Copy(rr, buf.Discard)
		common.Interrupt(s.input)
		s.Close()
		return drainErr
	}
	return err
}

func (m *ClientWorker) handleStatusEnd(meta *FrameMetadata, reader *buf.BufferedReader) error {
	if s, found := m.sessionManager.Get(meta.SessionID); found {
		// Mux.Pro: 使用 ErrorCode 而不是 OptionError 来判断关闭原因。
		if meta.ErrorCode != ErrorCodeGracefulShutdown {
			newError("session ", s.ID, " ended with error code: ", meta.ErrorCode).WriteToLog()
			common.Interrupt(s.input)
			common.Interrupt(s.output)
		}
		s.Close()
	}
	if meta.Option.Has(OptionData) {
		return buf.Copy(NewStreamReader(reader), buf.Discard)
	}
	return nil
}

func (m *ClientWorker) fetchOutput() {
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
		err := meta.Unmarshal(reader)
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
		case SessionStatusNegotiateVersion:
			// 协商后不应再收到此帧
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
