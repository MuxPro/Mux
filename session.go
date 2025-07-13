package mux

import (
	"sync"
	"sync/atomic" // Introduce atomic package for atomic operations

	"github.com/v2fly/v2ray-core/v5/common"
	"github.com/v2fly/v2ray-core/v5/common/buf"
	"github.com/v2fly/v2ray-core/v5/common/net" // For net.Destination
	"github.com/v2fly/v2ray-core/v5/common/protocol"
	"github.com/v2fly/v2ray-core/v5/common/signal/done" // For done.Instance
)

const (
	// DefaultInitialCredit is the initial sending credit for new sessions.
	DefaultInitialCredit uint32 = 64 * 1024 // 64KB
	// CreditUpdateThreshold is the received byte threshold that triggers a credit update.
	CreditUpdateThreshold uint32 = 32 * 1024 // 32KB
)

// GlobalID is an 8-byte identifier for UDP FullCone NAT sessions.
// This type is defined in frame.go. It's included here for clarity if this file is viewed in isolation.
// For a production build, ensure it's consistently defined in frame.go.
// type GlobalID [8]byte // Commented out as it's defined in frame.go

// SessionManager manages all sub-connections within a Mux main connection.
type SessionManager struct {
	sync.RWMutex
	sessions map[uint16]*Session
	count    uint16
	closed   bool
}

// NewSessionManager creates a new SessionManager.
func NewSessionManager() *SessionManager {
	return &SessionManager{
		count:    0,
		sessions: make(map[uint16]*Session, 16),
	}
}

// Closed checks if the SessionManager is closed.
func (m *SessionManager) Closed() bool {
	m.RLock()
	defer m.RUnlock()

	return m.closed
}

// Size returns the number of currently active sessions.
func (m *SessionManager) Size() int {
	m.RLock()
	defer m.RUnlock()

	return len(m.sessions)
}

// Count returns the total number of sessions allocated (including closed ones).
func (m *SessionManager) Count() int {
	m.RLock()
	defer m.RUnlock()

	return int(m.count)
}

// Allocate allocates a new Session ID and creates a Session.
func (m *SessionManager) Allocate() *Session {
	m.Lock()
	defer m.Unlock()

	if m.closed {
		return nil
	}

	for i := 0; i < 65535; i++ { // Iterate to find an unused ID
		m.count++
		if m.count == 0 { // Skip ID 0 as it's reserved for negotiation
			m.count++
		}
		if _, found := m.sessions[m.count]; !found {
			s := &Session{
				ID:            m.count,
				parent:        m,
				sendCredit:    DefaultInitialCredit,    // Mux.Pro: Initialize sending credit to default
				creditUpdate:  make(chan struct{}, 1),  // Mux.Pro: Initialize credit update notification channel
				receivedBytes: 0,                       // Mux.Pro: Initialize received bytes counter
				done:          done.New(),              // Initialize done instance for the session
			}
			m.sessions[s.ID] = s
			return s
		}
	}
	return nil // No available ID
}

// Add adds an existing Session to the manager.
// This is typically used when a session is created externally (e.g., by server upon receiving a New frame).
func (m *SessionManager) Add(s *Session) {
	m.Lock()
	defer m.Unlock()

	if m.closed {
		common.Close(s) // Close the session if manager is already closed
		return
	}

	s.parent = m
	// If s.done is not initialized, initialize it here.
	if s.done == nil {
		s.done = done.New()
	}
	m.sessions[s.ID] = s
	// Note: m.count is not incremented here as Allocate handles ID assignment.
	// If 's' is externally created, its ID should be unique.
}

// Remove removes a Session from the manager.
func (m *SessionManager) Remove(id uint16) {
	m.Lock()
	defer m.Unlock()

	if m.closed {
		return
	}

	if s, found := m.sessions[id]; found {
		// Mux.Pro: Close the creditUpdate channel to release waiting goroutines
		common.Close(s.creditUpdate) // Use common.Close to safely close channel
		delete(m.sessions, id)
	}

	if len(m.sessions) == 0 {
		m.sessions = make(map[uint16]*Session, 16) // Reset map to free memory
	}
}

// Get retrieves a Session by its ID.
func (m *SessionManager) Get(id uint16) (*Session, bool) {
	m.RLock()
	defer m.RUnlock()

	if m.closed {
		return nil, false
	}

	s, found := m.sessions[id]
	return s, found
}

// CloseIfNoSession closes the SessionManager if there are no active sessions.
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

// Close closes the SessionManager and all associated Sessions.
func (m *SessionManager) Close() error {
	m.Lock()
	defer m.Unlock()

	if m.closed {
		return nil
	}

	m.closed = true

	for _, s := range m.sessions {
		// Close session resources. Note: s.Close() calls m.Remove(s.ID), which acquires a lock.
		// To avoid deadlock, we close resources directly here and then clear the map.
		common.Close(s.input)
		common.Close(s.output)
		common.Close(s.done)
		common.Close(s.creditUpdate)
	}

	m.sessions = nil // Clear the map
	return nil
}

// Session represents a sub-connection within a Mux main connection.
type Session struct {
	ID           uint16
	input        buf.Reader
	output       buf.Writer
	parent       *SessionManager
	transferType protocol.TransferType
	done         *done.Instance // Signal when the session is closed

	// Mux.Pro: Flow control fields
	sendCredit    uint32        // Current available credit for sending data (atomic)
	creditUpdate  chan struct{} // Notification channel, signals when credit is updated
	receivedBytes uint32        // Bytes received by this side, used to determine when to send CreditUpdate (atomic)

	// Mux.Pro: For UDP FullCone NAT
	globalID       GlobalID      // GlobalID associated with this session (from client New frame)
	originalSource net.Destination // Original client UDP source address (from inbound context on server)
}

// Done returns a channel that signals when the session is closed.
func (s *Session) Done() <-chan struct{} {
	return s.done.Wait()
}

// AddReceivedBytes increases the count of received bytes for this session.
func (s *Session) AddReceivedBytes(n uint32) {
	atomic.AddUint32(&s.receivedBytes, n)
}

// GetReceivedBytes returns the total bytes received for this session since last reset.
func (s *Session) GetReceivedBytes() uint32 {
	return atomic.LoadUint32(&s.receivedBytes)
}

// ResetReceivedBytes resets the count of received bytes.
func (s *Session) ResetReceivedBytes() {
	atomic.StoreUint32(&s.receivedBytes, 0)
}

// Close closes all resources associated with this session.
func (s *Session) Close() error {
	// Remove from parent first to avoid deadlock if parent.Remove tries to close channel.
	if s.parent != nil {
		s.parent.Remove(s.ID)
	}
	common.Close(s.output)
	common.Close(s.input)
	common.Close(s.done) // Signal that the session is done
	common.Close(s.creditUpdate) // Ensure creditUpdate channel is closed
	return nil
}

// NewReader creates a buf.Reader based on the transfer type of this Session.
// NewStreamReader and NewPacketReader are now defined in reader.go.
func (s *Session) NewReader(reader *buf.BufferedReader) buf.Reader {
	if s.transferType == protocol.TransferTypeStream {
		return NewStreamReader(reader)
	}
	return NewPacketReader(reader)
}

// GrantCredit increases the session's sending credit.
func (s *Session) GrantCredit(increment uint32) {
	atomic.AddUint32(&s.sendCredit, increment)
	// Attempt to send a signal, skip if the channel is full to avoid blocking.
	select {
	case s.creditUpdate <- struct{}{}:
	default:
	}
}

// ConsumeCredit attempts to consume the session's sending credit.
// Returns the actual consumed bytes and whether consumption was successful.
func (s *Session) ConsumeCredit(amount uint32) (uint32, bool) {
	for {
		currentCredit := atomic.LoadUint32(&s.sendCredit)
		if currentCredit == 0 {
			return 0, false // No credit available to consume
		}

		// Calculate the actual amount that can be consumed
		consumeAmount := amount
		if currentCredit < amount {
			consumeAmount = currentCredit
		}

		// Attempt to atomically decrease credit
		if atomic.CompareAndSwapUint32(&s.sendCredit, currentCredit, currentCredit-consumeAmount) {
			return consumeAmount, true
		}
		// CAS failed, meaning credit was modified by another goroutine, retry
	}
}
