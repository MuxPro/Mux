package mux

import (
	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/common/buf"
	"github.com/v2fly/v2ray-core/v5/common/errors"
	"github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/common/protocol"
	"github.com/v2fly/v2ray-core/v5/common/serial"
)

// Writer 负责将应用层数据打包成 Mux 帧。
type Writer struct {
	dest         net.Destination
	writer       buf.Writer
	id           uint16
	followup     bool
	hasError     bool // Mux.Pro: 此字段将被 ErrorCode 取代，但暂时保留以兼容旧逻辑
	transferType protocol.TransferType
	priority     byte // Mux.Pro: 用于 New 帧的优先级
}

// NewWriter 创建一个新的 Writer，用于发送客户端发起的 Mux 帧。
// priority: Mux.Pro 新增字段，用于设置新连接的优先级。
func NewWriter(id uint16, dest net.Destination, writer buf.Writer, transferType protocol.TransferType, priority byte) *Writer {
	return &Writer{
		id:           id,
		dest:         dest,
		writer:       writer,
		followup:     false,
		transferType: transferType,
		priority:     priority, // Mux.Pro: 初始化优先级
	}
}

// NewResponseWriter 创建一个新的 Writer，用于发送服务器响应的 Mux 帧。
func NewResponseWriter(id uint16, writer buf.Writer, transferType protocol.TransferType) *Writer {
	return &Writer{
		id:           id,
		writer:       writer,
		followup:     true,
		transferType: transferType,
		priority:     0x00, // Mux.Pro: 响应Writer默认优先级为0
	}
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

// writeData 写入带有额外数据的 Mux 帧。
func (w *Writer) writeData(mb buf.MultiBuffer) error {
	meta := w.getNextFrameMeta()
	meta.Option.Set(OptionData) // 设置 OptionData 位，表示有额外数据

	return writeMetaWithFrame(w.writer, meta, mb)
}

// WriteMultiBuffer 实现 buf.Writer 接口，将 MultiBuffer 写入 Mux 帧。
func (w *Writer) WriteMultiBuffer(mb buf.MultiBuffer) error {
	defer buf.ReleaseMulti(mb)

	if mb.IsEmpty() {
		return w.writeMetaOnly()
	}

	for !mb.IsEmpty() {
		var chunk buf.MultiBuffer
		if w.transferType == protocol.TransferTypeStream {
			mb, chunk = buf.SplitSize(mb, 8*1024) // 流模式按大小分割
		} else {
			mb2, b := buf.SplitFirst(mb) // 包模式取第一个包
			mb = mb2
			chunk = buf.MultiBuffer{b}
		}
		if err := w.writeData(chunk); err != nil {
			return err
		}
	}

	return nil
}

// Close 实现 common.Closable 接口，关闭 Writer 并发送 End 帧。
// errorCode: Mux.Pro 新增参数，指定关闭子连接的原因。
func (w *Writer) Close(errorCode uint16) error {
	meta := FrameMetadata{
		SessionID:     w.id,
		SessionStatus: SessionStatusEnd,
	}

	// Mux.Pro: 如果错误码不是正常关闭，则设置 OptionData 位并包含错误码。
	// OptionError 在 Mux.Pro 中被 ErrorCode 机制取代。
	if errorCode != ErrorCodeGracefulShutdown {
		meta.Option.Set(OptionData) // D(0x01) 位在 End 帧中表示 ErrorCode 存在
		meta.ErrorCode = errorCode
	}

	frame := buf.New()
	common.Must(meta.WriteTo(frame))

	// 尝试写入 End 帧，不处理错误，因为此时连接可能已经断开。
	w.writer.WriteMultiBuffer(buf.MultiBuffer{frame})
	return nil
}

