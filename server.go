package mux

import (
	"context"
	"encoding/binary"
	"io"

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

// Server 实现了 Mux.Pro 服务器端逻辑。
type Server struct {
	dispatcher routing.Dispatcher
}

// NewServer 创建一个新的 Mux 服务器实例。
func NewServer(ctx context.Context) *Server {
	s := &Server{}
	core.RequireFeatures(ctx, func(d routing.Dispatcher) {
		s.dispatcher = d
	})
	return s
}

// Type 返回服务器的类型。
func (s *Server) Type() interface{} {
	return s.dispatcher.Type()
}

// Dispatch 处理入站连接。
func (s *Server) Dispatch(ctx context.Context, dest net.Destination) (*transport.Link, error) {
	// Mux.Pro: 检查目标地址是否为 "mux.pro"
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

// Start 启动服务器。
func (s *Server) Start() error {
	return nil
}

// Close 关闭服务器。
func (s *Server) Close() error {
	return nil
}

// ServerWorker 代表一个 Mux 主连接的服务器端。
type ServerWorker struct {
	dispatcher     routing.Dispatcher
	link           *transport.Link
	sessionManager *SessionManager
}

// NewServerWorker 创建一个新的 ServerWorker。
func NewServerWorker(ctx context.Context, d routing.Dispatcher, link *transport.Link) (*ServerWorker, error) {
	worker := &ServerWorker{
		dispatcher:     d,
		link:           link,
		sessionManager: NewSessionManager(),
	}
	go worker.run(ctx) // 启动主循环
	return worker, nil
}

// handle 处理子连接的数据转发。
func handle(ctx context.Context, s *Session, output buf.Writer) {
	writer := NewResponseWriter(s.ID, output, s.transferType)
	if err := buf.Copy(s.input, writer); err != nil {
		newError("session ", s.ID, " ends.").Base(err).WriteToLog(session.ExportIDToError(ctx))
		writer.Close(ErrorCodeProtocolError) // Mux.Pro: 发生错误时关闭，带上错误码
		return
	}

	writer.Close(ErrorCodeGracefulShutdown) // Mux.Pro: 正常关闭
	s.Close()
}

// ActiveConnections 返回活跃连接数。
func (w *ServerWorker) ActiveConnections() uint32 {
	return uint32(w.sessionManager.Size())
}

// Closed 检查工作器是否已关闭。
func (w *ServerWorker) Closed() bool {
	return w.sessionManager.Closed()
}

// negotiate 处理 Mux.Pro 版本协商。
func (w *ServerWorker) negotiate(reader *buf.BufferedReader) error {
	// 1. 读取客户端的协商请求
	var meta FrameMetadata
	if err := meta.Unmarshal(reader); err != nil {
		return newError("failed to read negotiation request metadata").Base(err)
	}

	// 检查 ID 和状态是否符合版本协商帧的要求
	if meta.SessionID != 0 || meta.SessionStatus != SessionStatusNegotiateVersion {
		return newError("unexpected first frame. expected negotiation, got: ID=", meta.SessionID, " Status=", meta.SessionStatus)
	}

	// 检查协商请求是否包含 Extra Data
	if !meta.Option.Has(OptionData) {
		return newError("negotiation request has no data")
	}

	// 读取客户端请求的 Extra Data 的长度
	dataLen, err := serial.ReadUint16(reader)
	if err != nil {
		return newError("failed to read negotiation request data length").Base(err)
	}

	// 读取客户端请求的 Extra Data
	requestPayload := buf.New()
	defer requestPayload.Release()
	if _, err := requestPayload.ReadFullFrom(reader, int32(dataLen)); err != nil {
		return err
	}

	// 解析客户端版本列表: [1字节版本数量 N] [N * 4字节版本号]
	count := requestPayload.Byte(0)
	requestPayload.Advance(1) // 消耗版本数量字节

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
	supportedVersions := []uint32{Version} // 服务器支持的版本列表 (目前只有 Mux.Pro 0.0)

	// 遍历客户端版本，寻找最高共同版本
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

	// 3. 发送协商成功的响应
	responseMeta := FrameMetadata{
		SessionID:     0,
		SessionStatus: SessionStatusNegotiateVersion,
		Option:        OptionData, // 必须设置 OptionData，因为响应包含协商版本
	}

	frame := buf.New()
	common.Must(responseMeta.WriteTo(frame))

	// 构造响应 Extra Data: [1字节版本数量 N=1] [1 * 4字节协商版本号]
	versionsPayload := buf.New()
	defer versionsPayload.Release()
	common.Must(versionsPayload.WriteByte(1)) // 版本数量 N = 1
	common.Must(WriteUint32(versionsPayload, negotiatedVersion))

	Must2(serial.WriteUint16(frame, uint16(versionsPayload.Len()))) // 写入 Extra Data 长度
	Must2(frame.Write(versionsPayload.Bytes()))                     // 写入 Extra Data 内容

	if err := w.link.Writer.WriteMultiBuffer(buf.MultiBuffer{frame}); err != nil {
		return newError("failed to write negotiation response").Base(err)
	}

	newError("Mux.Pro negotiation succeeded, version ", negotiatedVersion).WriteToLog()
	return nil
}

// handleStatusKeepAlive 处理 KeepAlive 帧。
func (w *ServerWorker) handleStatusKeepAlive(meta *FrameMetadata, reader *buf.BufferedReader) error {
	// Mux.Pro: KeepAlive 帧的 Opt 必须为 0x00，且不带数据。
	if meta.Option != 0 {
		return newError("protocol error: KeepAlive frame with non-zero option: ", meta.Option)
	}
	// KeepAlive 帧不应有 Extra Data，这里不需要额外处理 Extra Data。
	return nil
}

// handleStatusNew 处理 New 帧，创建新的子连接。
func (w *ServerWorker) handleStatusNew(ctx context.Context, meta *FrameMetadata, reader *buf.BufferedReader) error {
	// Mux.Pro: 记录新连接的优先级
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
		if meta.Option.Has(OptionData) { // 如果 New 帧包含初始数据，丢弃它
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
	}
	if meta.Target.Network == net.Network_UDP {
		s.transferType = protocol.TransferTypePacket
	}
	w.sessionManager.Add(s)
	go handle(ctx, s, w.link.Writer) // 启动子连接的数据转发
	if !meta.Option.Has(OptionData) {
		return nil
	}

	rr := s.NewReader(reader)
	if err := buf.Copy(rr, s.output); err != nil {
		buf.Copy(rr, buf.Discard) // 丢弃剩余数据
		common.Interrupt(s.input)
		return s.Close()
	}
	return nil
}

// handleStatusKeep 处理 Keep 帧（数据传输）。
func (w *ServerWorker) handleStatusKeep(meta *FrameMetadata, reader *buf.BufferedReader) error {
	if !meta.Option.Has(OptionData) {
		return nil
	}

	s, found := w.sessionManager.Get(meta.SessionID)
	if !found {
		closingWriter := NewResponseWriter(meta.SessionID, w.link.Writer, protocol.TransferTypeStream)
		closingWriter.Close() // 关闭一个不存在的会话
		return buf.Copy(NewStreamReader(reader), buf.Discard)
	}

	rr := s.NewReader(reader)
	err := buf.Copy(rr, s.output) // 将数据从 Mux Reader 复制到会话输出流
	if err != nil && buf.IsWriteError(err) {
		newError("failed to write to downstream writer. closing session ", s.ID).Base(err).WriteToLog()
		closingWriter := NewResponseWriter(meta.SessionID, w.link.Writer, protocol.TransferTypeStream)
		closingWriter.Close()
		drainErr := buf.Copy(rr, buf.Discard) // 丢弃剩余数据
		common.Interrupt(s.input)
		s.Close()
		return drainErr
	}
	return err
}

// handleStatusEnd 处理 End 帧（关闭子连接）。
func (w *ServerWorker) handleStatusEnd(meta *FrameMetadata, reader *buf.BufferedReader) error {
	if s, found := w.sessionManager.Get(meta.SessionID); found {
		// Mux.Pro: 使用 ErrorCode 而不是 OptionError 来判断关闭原因。
		if meta.ErrorCode != ErrorCodeGracefulShutdown {
			newError("session ", s.ID, " ended with error code: ", meta.ErrorCode).WriteToLog()
			common.Interrupt(s.input)
			common.Interrupt(s.output)
		}
		s.Close()
	}
	if meta.Option.Has(OptionData) { // 如果 End 帧意外地包含数据，丢弃它
		return buf.Copy(NewStreamReader(reader), buf.Discard)
	}
	return nil
}

// handleFrame 处理接收到的 Mux 帧。
func (w *ServerWorker) handleFrame(ctx context.Context, reader *buf.BufferedReader) error {
	var meta FrameMetadata
	err := meta.Unmarshal(reader) // 解析帧元数据
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
	case SessionStatusNegotiateVersion:
		// 协商后不应再收到此帧，视为协议错误
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

// run 是 ServerWorker 的主循环，负责读取和处理入站 Mux 帧。
func (w *ServerWorker) run(ctx context.Context) {
	input := w.link.Reader
	reader := &buf.BufferedReader{Reader: input}

	defer w.sessionManager.Close()

	// Mux.Pro: 在处理任何其他帧之前，首先执行版本协商。
	if err := w.negotiate(reader); err != nil {
		newError("Mux.Pro negotiation failed").Base(err).WriteToLog(session.ExportIDToError(ctx))
		common.Interrupt(input) // 协商失败，中断底层连接
		return
	}

	for {
		select {
		case <-ctx.Done(): // 上下文取消信号
			return
		default:
			err := w.handleFrame(ctx, reader) // 处理下一帧
			if err != nil {
				if errors.Cause(err) != io.EOF {
					newError("worker run loop ended").Base(err).WriteToLog(session.ExportIDToError(ctx))
					common.Interrupt(input) // 发生错误，中断底层连接
				}
				return
			}
		}
	}
}

