package mux

import (
	"time" // 引入 time 包用于超时
	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/common/buf"
	"github.com/v2fly/v2ray-core/v5/common/errors"
	"github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/common/protocol"
	"github.com/v2fly/v2ray-core/v5/common/serial"
)

// Writer 负责将应用层数据打包成 Mux 帧。
type Writer struct {
	dest             net.Destination
	writer           buf.Writer
	id               uint16
	followup         bool
	transferType     protocol.TransferType
	priority         byte
	currentErrorCode uint16
	session          *Session // Mux.Pro: 引用关联的 Session，用于流控
}

// NewWriter 创建一个新的 Writer，用于发送客户端发起的 Mux 帧。
// priority: Mux.Pro 新增字段，用于设置新连接的优先级。
// s: Mux.Pro 新增参数，指向此 Writer 关联的 Session，用于流控。
func NewWriter(id uint16, dest net.Destination, writer buf.Writer, transferType protocol.TransferType, priority byte, s *Session) *Writer {
	return &Writer{
		id:               id,
		dest:             dest,
		writer:           writer,
		followup:         false,
		transferType:     transferType,
		priority:         priority,
		currentErrorCode: ErrorCodeGracefulShutdown, // Mux.Pro: 默认正常关闭
		session:          s,                         // Mux.Pro: 关联 Session
	}
}

// NewResponseWriter 创建一个新的 Writer，用于发送服务器响应的 Mux 帧。
// s: Mux.Pro 新增参数，指向此 Writer 关联的 Session，用于流控。
func NewResponseWriter(id uint16, writer buf.Writer, transferType protocol.TransferType, s *Session) *Writer {
	return &Writer{
		id:               id,
		writer:           writer,
		followup:         true,
		transferType:     transferType,
		priority:         0x00,                        // Mux.Pro: 响应Writer默认优先级为0
		currentErrorCode: ErrorCodeGracefulShutdown, // Mux.Pro: 默认正常关闭
		session:          s,                         // Mux.Pro: 关联 Session
	}
}

// SetErrorCode 设置 Writer 在关闭时使用的错误码。
func (w *Writer) SetErrorCode(code uint16) {
	w.currentErrorCode = code
}

// getNextFrameMeta 生成下一个 Mux 帧的元数据。
func (w *Writer) getNextFrameMeta() FrameMetadata {
	meta := FrameMetadata{
		SessionID: w.id,
		Target:    w.dest,
	}

	if w.followup {
		meta.SessionStatus = SessionStatusKeep
	} else {
		w.followup = true
		meta.SessionStatus = SessionStatusNew
		// Mux.Pro: 对于 SessionStatusNew 帧，设置优先级
		meta.Priority = w.priority
	}

	return meta
}

// writeMetaOnly 仅发送元数据帧（不带额外数据）。
func (w *Writer) writeMetaOnly() error {
	meta := w.getNextFrameMeta()
	b := buf.New()
	if err := meta.WriteTo(b); err != nil {
		return err
	}
	return w.writer.WriteMultiBuffer(buf.MultiBuffer{b})
}

// writeMetaWithFrame 写入元数据和额外数据。
func writeMetaWithFrame(writer buf.Writer, meta FrameMetadata, data buf.MultiBuffer) error {
	frame := buf.New()
	if err := meta.WriteTo(frame); err != nil {
		return err
	}
	// Mux.Pro: Extra Data 自身也带有长度字段
	if _, err := serial.WriteUint16(frame, uint16(data.Len())); err != nil {
		return err
	}

	if len(data)+1 > 64*1024*1024 { // 检查总长度是否过大
		return errors.New("value too large")
	}
	sliceSize := len(data) + 1
	mb2 := make(buf.MultiBuffer, 0, sliceSize)
	mb2 = append(mb2, frame)
	mb2 = append(mb2, data...)
	return writer.WriteMultiBuffer(mb2)
}

// writeDataInternal 是实际写入数据帧的内部函数，不包含流控逻辑。
func (w *Writer) writeDataInternal(mb buf.MultiBuffer) error {
	meta := w.getNextFrameMeta()
	meta.Option.Set(OptionData) // 设置 OptionData 位，表示有额外数据
	return writeMetaWithFrame(w.writer, meta, mb)
}

// WriteMultiBuffer 实现 buf.Writer 接口，将 MultiBuffer 写入 Mux 帧。
// 包含了 Mux.Pro 的信用流控逻辑。
func (w *Writer) WriteMultiBuffer(mb buf.MultiBuffer) error {
	defer buf.ReleaseMulti(mb)

	if mb.IsEmpty() {
		return w.writeMetaOnly()
	}

	for !mb.IsEmpty() {
		var chunk buf.MultiBuffer
		if w.transferType == protocol.TransferTypeStream {
			// 流模式按 8KB 分割，或者剩余所有数据
			if mb.Len() > 8*1024 {
				mb, chunk = buf.SplitSize(mb, 8*1024)
			} else {
				chunk = mb
				mb = nil // 所有数据都已处理
			}
		} else {
			// 包模式取第一个包
			mb2, b := buf.SplitFirst(mb)
			mb = mb2
			chunk = buf.MultiBuffer{b}
		}

		// Mux.Pro: Credit-based flow control for each chunk
		chunkLen := uint32(chunk.Len())
		if chunkLen == 0 { // 空块，直接跳过
			continue
		}

		for { // 循环直到成功消费信用并发送数据
			consumed, ok := w.session.ConsumeCredit(chunkLen)
			if ok {
				// 成功消费了信用，发送相应部分的数据
				partToSend := buf.SplitSize(chunk, int32(consumed))
				if err := w.writeDataInternal(partToSend); err != nil {
					return err
				}
				chunk = chunk.SliceFrom(int32(consumed)) // 更新 chunk 为剩余未发送的数据
				chunkLen -= consumed
				if chunkLen == 0 { // 当前 chunk 已全部发送
					break // 跳出内部循环，处理下一个原始 mb 的 chunk
				}
			} else {
				// 没有信用可消费，等待信用更新信号
				select {
				case <-w.session.creditUpdate:
					// 收到信用更新信号，继续尝试消费信用
					continue
				case <-w.session.parent.CloseIfNoSession(): // 检查 SessionManager 是否正在关闭
					// 如果 SessionManager 正在关闭，则停止等待并返回错误
					return newError("session manager closing while waiting for credit for session ", w.id).AtWarning()
				case <-time.After(time.Second * 30): // 设置超时，防止无限期阻塞
					return newError("writer for session ", w.id, " blocked due to no credit, timed out").AtWarning()
				}
			}
		}
	}

	return nil
}

// Close 实现 common.Closable 接口，关闭 Writer 并发送 End 帧。
func (w *Writer) Close() error {
	meta := FrameMetadata{
		SessionID:     w.id,
		SessionStatus: SessionStatusEnd,
	}

	// Mux.Pro: 如果错误码不是正常关闭，则设置 OptionData 位并包含错误码。
	if w.currentErrorCode != ErrorCodeGracefulShutdown {
		meta.Option.Set(OptionData) // D(0x01) 位在 End 帧中表示 ErrorCode 存在
		meta.ErrorCode = w.currentErrorCode
	}

	frame := buf.New()
	common.Must(meta.WriteTo(frame))

	// 尝试写入 End 帧，不处理错误，因为此时连接可能已经断开。
	w.writer.WriteMultiBuffer(buf.MultiBuffer{frame})
	return nil
}

