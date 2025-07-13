package mux

import (
	"sync"

	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/common/buf"
	"github.com/v2fly/v2ray-core/v5/common/protocol"
)

type SessionManager struct {
	sync.RWMutex
	sessions map[uint16]*Session
	count    uint16 // Next available session ID
	closed   bool
}

func NewSessionManager() *SessionManager {
	return &SessionManager{
		count:    0, // Will be incremented to 1 for the first session
		sessions: make(map[uint16]*Session, 16),
	}
}

func (m *SessionManager) Closed() bool {
	m.RLock()
	defer m.RUnlock()

	return m.closed
}

func (m *SessionManager) Size() int {
	m.RLock()
	defer m.RUnlock()

	return len(m.sessions)
}

func (m *SessionManager) Count() int {
	m.RLock()
	defer m.RUnlock()

	return int(m.count)
}

// Allocate allocates a new Session ID and creates a Session.
// Mux.Pro IDs are 2-byte unsigned integers, 0x0000 is reserved for Main Connection Control.
// So, IDs for sub-connections should start from 0x0001.
func (m *SessionManager) Allocate() *Session {
	m.Lock()
	defer m.Unlock()

	if m.closed {
		return nil
	}

	// Find the next available ID that is not 0x0000
	for {
		m.count++ // Increment to get a new ID
		if m.count == 0 { // Skip 0x0000 as it's reserved for Main Connection Control
			m.count++
		}
		if _, exists := m.sessions[m.count]; !exists {
			break // Found an unused ID
		}
		if m.count == 0xFFFF { // If wrapped around, and 0xFFFF is also used, try to find one
			// This scenario is highly unlikely for 65534 concurrent sessions.
			// For simplicity, we just return nil, indicating no more IDs.
			// In a real-world scenario, you might want to log a warning or
			// implement more sophisticated ID recycling/management.
			return nil
		}
	}

	s := &Session{
		ID:     m.count,
		parent: m,
		credit: initialSessionCredit, // Mux.Pro: Initialize session credit
	}
	m.sessions[s.ID] = s
	return s
}

func (m *SessionManager) Add(s *Session) {
	m.Lock()
	defer m.Unlock()

	if m.closed {
		return
	}
	m.sessions[s.ID] = s
}

func (m *SessionManager) Remove(id uint16) {
	m.Lock()
	defer m.Unlock()

	if m.closed {
		return
	}

	delete(m.sessions, id)

	if len(m.sessions) == 0 {
		m.sessions = make(map[uint16]*Session, 16)
	}
}

func (m *SessionManager) Get(id uint16) (*Session, bool) {
	m.RLock()
	defer m.RUnlock()

	if m.closed {
		return nil, false
	}

	s, found := m.sessions[id]
	return s, found
}

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

	// Mux.Pro Extension: Flow control credit
	creditMtx sync.Mutex
	credit    uint32
}

// Default initial credit for a new session.
// This value can be tuned based on network conditions and desired throughput.
const initialSessionCredit uint32 = 64 * 1024 // 64KB initial credit

// AddCredit adds credit to the session.
func (s *Session) AddCredit(amount uint32) {
	s.creditMtx.Lock()
	defer s.creditMtx.Unlock()
	s.credit += amount
}

// ConsumeCredit attempts to consume credit from the session.
// Returns true if enough credit is available and consumed, false otherwise.
func (s *Session) ConsumeCredit(amount uint32) bool {
	s.creditMtx.Lock()
	defer s.creditMtx.Unlock()
	if s.credit >= amount {
		s.credit -= amount
		return true
	}
	return false
}

// HasCredit checks if the session has at least the specified amount of credit.
func (s *Session) HasCredit(amount uint32) bool {
	s.creditMtx.Lock()
	defer s.creditMtx.Unlock()
	return s.credit >= amount
}

// GetCredit returns the current credit amount for the session.
func (s *Session) GetCredit() uint32 {
	s.creditMtx.Lock()
	defer s.creditMtx.Unlock()
	return s.credit
}

// Close closes the session.
func (s *Session) Close() error {
	common.Close(s.input)
	common.Close(s.output)
	s.parent.Remove(s.ID)
	return nil
}

func (s *Session) NewReader(reader *buf.BufferedReader) buf.Reader {
	if s.transferType == protocol.TransferTypeStream {
		return crypto.NewChunkStreamReaderWithChunkCount(reader)
	}
	return NewPacketReader(reader)
}
