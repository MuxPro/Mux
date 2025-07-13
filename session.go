package mux

import (
	"sync"
	"sync/atomic" // 引入 atomic 包用于原子操作

	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/common/buf"
	"github.com/v2fly/v2ray-core/v5/common/protocol"
)

const (
	// DefaultInitialCredit 是新会话的初始发送信用。
	DefaultInitialCredit uint32 = 64 * 1024 // 64KB
	// CreditUpdateThreshold 是触发信用更新的接收字节阈值。
	CreditUpdateThreshold uint32 = 32 * 1024 // 32KB
)

// SessionManager 管理 Mux 主连接中的所有子连接。
type SessionManager struct {
	sync.RWMutex
	sessions map[uint16]*Session
	count    uint16
	closed   bool
}

// NewSessionManager 创建一个新的 SessionManager。
func NewSessionManager() *SessionManager {
	return &SessionManager{
		count:    0,
		sessions: make(map[uint16]*Session, 16),
	}
}

// Closed 检查 SessionManager 是否已关闭。
func (m *SessionManager) Closed() bool {
	m.RLock()
	defer m.RUnlock()

	return m.closed
}

// Size 返回当前活跃会话的数量。
func (m *SessionManager) Size() int {
	m.RLock()
	defer m.RUnlock()

	return len(m.sessions)
}

// Count 返回已分配的会话总数（包括已关闭的）。
func (m *SessionManager) Count() int {
	m.RLock()
	defer m.RUnlock()

	return int(m.count)
}

// Allocate 分配一个新的 Session ID 并创建 Session。
func (m *SessionManager) Allocate() *Session {
	m.Lock()
	defer m.Unlock()

	if m.closed {
		return nil
	}

	m.count++
	s := &Session{
		ID:           m.count,
		parent:       m,
		sendCredit:   DefaultInitialCredit, // Mux.Pro: 初始化发送信用为默认值
		creditUpdate: make(chan struct{}, 1), // Mux.Pro: 初始化信用更新通知通道
		receivedBytes: 0, // Mux.Pro: 初始化接收字节计数
	}
	m.sessions[s.ID] = s
	return s
}

// Add 将一个已存在的 Session 添加到管理器中。
func (m *SessionManager) Add(s *Session) {
	m.Lock()
	defer m.Unlock()

	if m.closed {
		return
	}

	// 注意：这里没有递增 m.count，因为 Allocate 已经处理了 ID 分配。
	// 如果 s 是外部创建的，需要确保其 ID 唯一。
	m.sessions[s.ID] = s
}

// Remove 从管理器中移除一个 Session。
func (m *SessionManager) Remove(id uint16) {
	m.Lock()
	defer m.Unlock()

	if m.closed {
		return
	}

	// Mux.Pro: 关闭信用更新通道以释放等待的 goroutine
	if s, found := m.sessions[id]; found {
		select { // 避免关闭已关闭的通道
		case <-s.creditUpdate:
		default:
		}
		close(s.creditUpdate)
	}

	delete(m.sessions, id)

	if len(m.sessions) == 0 {
		m.sessions = make(map[uint16]*Session, 16)
	}
}

// Get 根据 ID 获取一个 Session。
func (m *SessionManager) Get(id uint16) (*Session, bool) {
	m.RLock()
	defer m.RUnlock()

	if m.closed {
		return nil, false
	}

	s, found := m.sessions[id]
	return s, found
}

// CloseIfNoSession 如果没有活跃会话，则关闭 SessionManager。
func (m *SessionManager) CloseIfNoSession() bool {
	m.Lock()
	defer m.Unlock()

	if m.closed {
		return true
	}

	if len(m.sessions) != 0 {
		return false
	}

	m.closed = true
	return true
}

// Close 关闭 SessionManager 和所有关联的 Session。
func (m *SessionManager) Close() error {
	m.Lock()
	defer m.Unlock()

	if m.closed {
		return nil
	}

	m.closed = true

	for _, s := range m.sessions {
		common.Close(s.input)
		common.Close(s.output)
		// Mux.Pro: 关闭信用更新通道
		select { // 避免关闭已关闭的通道
		case <-s.creditUpdate:
		default:
		}
		close(s.creditUpdate)
	}

	m.sessions = nil
	return nil
}

// Session represents a client connection in a Mux connection.
type Session struct {
	input        buf.Reader
	output       buf.Writer
	parent       *SessionManager
	ID           uint16
	transferType protocol.TransferType

	// Mux.Pro: 流控相关字段
	sendCredit    uint32        // 我方可以发送的字节数 (原子操作)
	creditUpdate  chan struct{} // 通知通道，当信用更新时发送信号
	receivedBytes uint32        // 我方已接收的字节数，用于判断何时发送 CreditUpdate (原子操作)
}

// Close closes all resources associated with this session.
func (s *Session) Close() error {
	s.parent.Remove(s.ID) // 先从管理器中移除，这样 Remove 会关闭 creditUpdate channel
	common.Close(s.output)
	common.Close(s.input)
	return nil
}

// NewReader creates a buf.Reader based on the transfer type of this Session.
func (s *Session) NewReader(reader *buf.BufferedReader) buf.Reader {
	if s.transferType == protocol.TransferTypeStream {
		return NewStreamReader(reader)
	}
	return NewPacketReader(reader)
}

// GrantCredit 增加会话的发送信用。
func (s *Session) GrantCredit(increment uint32) {
	atomic.AddUint32(&s.sendCredit, increment)
	// 尝试发送信号，如果通道已满则跳过，避免阻塞
	select {
	case s.creditUpdate <- struct{}{}:
	default:
	}
}

// ConsumeCredit 尝试消费会话的发送信用。
// 返回实际消费的字节数和是否成功消费。
func (s *Session) ConsumeCredit(amount uint32) (uint32, bool) {
	for {
		currentCredit := atomic.LoadUint32(&s.sendCredit)
		if currentCredit == 0 {
			return 0, false // 没有信用可消费
		}

		// 计算实际可以消费的量
		consumeAmount := amount
		if currentCredit < amount {
			consumeAmount = currentCredit
		}

		// 尝试原子地减少信用
		if atomic.CompareAndSwapUint32(&s.sendCredit, currentCredit, currentCredit-consumeAmount) {
			return consumeAmount, true
		}
		// CAS 失败，说明 credit 已经被其他 goroutine 修改，重试
	}
}

// AddReceivedBytes 增加会话的接收字节计数。
func (s *Session) AddReceivedBytes(count uint32) {
	atomic.AddUint32(&s.receivedBytes, count)
}

// GetReceivedBytes 获取当前会话的接收字节计数。
func (s *Session) GetReceivedBytes() uint32 {
	return atomic.LoadUint32(&s.receivedBytes)
}

// ResetReceivedBytes 重置会话的接收字节计数。
func (s *Session) ResetReceivedBytes() {
	atomic.StoreUint32(&s.receivedBytes, 0)
}

