package mux

import (
	"encoding/binary"
	"io"

	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/common/bitmask"
	"github.com/v2fly/v2ray-core/v5/common/buf"
	"github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/common/protocol"
	"github.com/v2fly/v2ray-core/v5/common/serial"
)

// Mux.Pro protocol version 0.0.
// Major (2 bytes) | Minor (2 bytes)
const (
	Version uint32 = 0x00000000
)

// SessionStatus 定义了帧的类型和用途。
type SessionStatus byte

const (
	SessionStatusNegotiateVersion SessionStatus = 0x00 // Mux.Pro: 版本协商
	SessionStatusNew              SessionStatus = 0x01 // 新建子连接
	SessionStatusKeep             SessionStatus = 0x02 // 传输数据
	SessionStatusEnd              SessionStatus = 0x03 // 关闭子连接
	SessionStatusKeepAlive        SessionStatus = 0x04 // 保持主连接
	SessionStatusCreditUpdate     SessionStatus = 0x05 // Mux.Pro: 信用更新 (流控)
)

// Option 定义了元数据中的选项位。
const (
	// D (0x01): Data Present.
	// 对于数据帧 (New, Keep)，表示有 Extra Data。
	// 对于 End 帧，表示有 ErrorCode。
	// 对于 NegotiateVersion 和 CreditUpdate 帧，表示有 Extra Data。
	OptionData bitmask.Byte = 0x01

	// Mux.Cool 中用于表示错误的位，在 Mux.Pro 中被 End 帧的 ErrorCode 机制取代。
	OptionError bitmask.Byte = 0x02
)

// ErrorCode... 定义了 Mux.Pro End 帧中的错误码。
const (
	ErrorCodeGracefulShutdown   uint16 = 0x0000 // 正常关闭
	ErrorCodeRemoteDisconnect   uint16 = 0x0001 // 远端目标断开连接
	ErrorCodeOperationTimeout   uint16 = 0x0002 // 操作超时
	ErrorCodeProtocolError      uint16 = 0x0003 // 协议错误
	ErrorCodeResourceExhaustion uint16 = 0x0004 // 资源耗尽
	ErrorCodeDestUnreachable    uint16 = 0x0005 // 目标不可达
	ErrorCodeProxyRejected      uint16 = 0x0006 // 代理拒绝
)

type TargetNetwork byte

const (
	TargetNetworkTCP TargetNetwork = 0x01
	TargetNetworkUDP TargetNetwork = 0x02
)

var addrParser = protocol.NewAddressParser(
	protocol.AddressFamilyByte(byte(protocol.AddressTypeIPv4), net.AddressFamilyIPv4),
	protocol.AddressFamilyByte(byte(protocol.AddressTypeDomain), net.AddressFamilyDomain),
	protocol.AddressFamilyByte(byte(protocol.AddressTypeIPv6), net.AddressFamilyIPv6),
	protocol.PortThenAddress(),
)

/*
Mux.Pro Frame 格式
+-------------------+-----------------+----------------+
| 2 字节            | L 字节          | X 字节         |
+-------------------+-----------------+----------------+
| 元数据长度 L      | 元数据          | 额外数据       |
+-------------------+-----------------+----------------+

元数据 (Metadata) 格式
+-----------+---------------+-------------------+---------------------+
| 2 字节    | 1 字节        | 1 字节            | 可变长度            |
+-----------+---------------+-------------------+---------------------+
| ID        | 状态 S        | 选项 Opt          | 选项/字段...        |
+-----------+---------------+-------------------+---------------------+
*/

// FrameMetadata 代表一个 Mux 帧的元数据部分。
type FrameMetadata struct {
	Target        net.Destination
	SessionID     uint16
	Option        bitmask.Byte
	SessionStatus SessionStatus

	// Mux.Pro 新增字段
	Priority  byte   // 用于 New 帧
	ErrorCode uint16 // 用于 End 帧
}

// WriteTo 将元数据序列化到缓冲区中。
func (f *FrameMetadata) WriteTo(b *buf.Buffer) error {
	lenBytes := b.Extend(2)
	len0 := b.Len()

	sessionBytes := b.Extend(2)
	binary.BigEndian.PutUint16(sessionBytes, f.SessionID)

	common.Must(b.WriteByte(byte(f.SessionStatus)))
	common.Must(b.WriteByte(byte(f.Option)))

	switch f.SessionStatus {
	case SessionStatusNew:
		switch f.Target.Network {
		case net.Network_TCP:
			common.Must(b.WriteByte(byte(TargetNetworkTCP)))
		case net.Network_UDP:
			common.Must(b.WriteByte(byte(TargetNetworkUDP)))
		}
		// Mux.Pro: 写入优先级字段
		common.Must(b.WriteByte(f.Priority))

		if err := addrParser.WriteAddressPort(b, f.Target.Address, f.Target.Port); err != nil {
			return err
		}
	case SessionStatusEnd:
		// Mux.Pro: 对于 End 帧, Opt(D) 表示 ErrorCode 存在。
		if f.Option.Has(OptionData) {
			if err := WriteUint16(b, f.ErrorCode); err != nil {
				return err
			}
		}
	}

	len1 := b.Len()
	binary.BigEndian.PutUint16(lenBytes, uint16(len1-len0))
	return nil
}

// Unmarshal 从 reader 中读取并解析元数据。
func (f *FrameMetadata) Unmarshal(reader io.Reader) error {
	metaLen, err := serial.ReadUint16(reader)
	if err != nil {
		return err
	}
	if metaLen > 512 {
		return newError("invalid metalen ", metaLen).AtError()
	}

	b := buf.New()
	defer b.Release()

	if _, err := b.ReadFullFrom(reader, int32(metaLen)); err != nil {
		return err
	}
	return f.UnmarshalFromBuffer(b)
}

// UnmarshalFromBuffer 从给定的缓冲区中解析元数据。
func (f *FrameMetadata) UnmarshalFromBuffer(b *buf.Buffer) error {
	if b.Len() < 4 {
		return newError("insufficient buffer for metadata header: ", b.Len())
	}

	f.SessionID = binary.BigEndian.Uint16(b.Bytes())
	f.SessionStatus = SessionStatus(b.Byte(2))
	f.Option = bitmask.Byte(b.Byte(3))
	b.Advance(4) // 消耗已读取的头部

	// 设置默认值
	f.Target.Network = net.Network_Unknown
	f.ErrorCode = ErrorCodeGracefulShutdown

	switch f.SessionStatus {
	case SessionStatusNew:
		// 至少需要1字节网络类型和1字节优先级
		if b.Len() < 2 {
			return newError("insufficient buffer for New frame fields: ", b.Len())
		}
		network := TargetNetwork(b.Byte(0))
		// Mux.Pro: 读取优先级字段
		f.Priority = b.Byte(1)
		b.Advance(2)

		addr, port, err := addrParser.ReadAddressPort(nil, b)
		if err != nil {
			return newError("failed to parse address and port for New frame").Base(err)
		}

		switch network {
		case TargetNetworkTCP:
			f.Target = net.TCPDestination(addr, port)
		case TargetNetworkUDP:
			f.Target = net.UDPDestination(addr, port)
		default:
			return newError("unknown network type: ", network)
		}
	case SessionStatusEnd:
		// Mux.Pro: 对于 End 帧, Opt(D) 表示 ErrorCode 存在。
		if f.Option.Has(OptionData) {
			if b.Len() < 2 {
				return newError("insufficient buffer for End frame error code: ", b.Len())
			}
			f.ErrorCode = binary.BigEndian.Uint16(b.Bytes())
			b.Advance(2)
		}
	}

	return nil
}
