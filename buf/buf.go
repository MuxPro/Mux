package buf

import (
	"io"
	"sync/atomic"
	"time" // 引入 time 包

	"github.com/v2fly/v2ray-core/v5/common/errors"
	"github.com/v2fly/v2ray-core/v5/common/net" // 引入 net 包
	"github.com/v2fly/v2ray-core/v5/common/serial"
)

// Size is the size of a Buffer.
const Size = 1400 // Standard MTU for Ethernet minus IP/UDP headers, common for UDP packets

// Buffer is a piece of reusable memory.
type Buffer struct {
	// data stores the actual bytes in this Buffer.
	data []byte
	// start is the starting offset of the content in data.
	start int32
	// end is the ending offset of the content in data.
	end int32

	// Mux.Pro: For UDP packets, carries the associated address information.
	// This will be set by PacketReader on inbound UDP Keep frames,
	// and used by Writer for outbound UDP Keep frames.
	UDPInfo *net.Destination
	// Mux.Pro: For UDP packets, carries the associated GlobalID.
	// Note: GlobalID type needs to be defined in a common place accessible by both mux and buf packages.
	// For now, assuming it's defined in mux package and imported, or a placeholder.
	// Given the context, GlobalID is defined in mux/frame.go, so we need to import mux for it.
	// However, buf package should ideally not depend on mux package directly.
	// For compilation, I will temporarily define GlobalID here or pass it as an interface{}.
	// A better solution would be to pass GlobalID as a byte slice or define it in a truly common package.
	// For now, to make it compile, I'll use a placeholder type if mux.GlobalID is not accessible.
	// Let's assume GlobalID is a simple [8]byte for now to avoid circular dependency.
	GlobalID [8]byte // Placeholder for GlobalID, assuming it's an 8-byte array
}

// New creates a new Buffer.
func New() *Buffer {
	// In a real implementation, this would come from a buffer pool.
	// For this example, we'll just allocate a new slice.
	return &Buffer{
		data: make([]byte, Size),
	}
}

// Release releases the Buffer back to the pool.
func (b *Buffer) Release() {
	// In a real implementation, this would return the buffer to a pool.
	b.data = nil
	b.start = 0
	b.end = 0
	b.UDPInfo = nil
	b.GlobalID = [8]byte{} // Reset GlobalID
}

// IsEmpty returns true if the Buffer has no content.
func (b *Buffer) IsEmpty() bool {
	return b.Len() == 0
}

// Len returns the length of the content in the Buffer.
func (b *Buffer) Len() int32 {
	return b.end - b.start
}

// Cap returns the total capacity of the Buffer.
func (b *Buffer) Cap() int32 {
	return int32(len(b.data))
}

// Bytes returns the content of the Buffer as a byte slice.
func (b *Buffer) Bytes() []byte {
	return b.data[b.start:b.end]
}

// BytesTo returns a slice of the content up to 'size' bytes.
func (b *Buffer) BytesTo(size int32) []byte {
	if size > b.Len() {
		size = b.Len()
	}
	return b.data[b.start : b.start+size]
}

// BytesFrom returns a slice of the content from 'size' bytes to the end.
func (b *Buffer) BytesFrom(size int32) []byte {
	if size > b.Len() {
		size = b.Len()
	}
	return b.data[b.start+size : b.end]
}

// Extend extends the Buffer by 'size' bytes and returns the extended slice.
func (b *Buffer) Extend(size int32) []byte {
	if b.end+size > b.Cap() {
		panic("buffer overflow") // In a real system, this would reallocate or get a new buffer
	}
	oldEnd := b.end
	b.end += size
	return b.data[oldEnd:b.end]
}

// Write appends bytes to the Buffer.
func (b *Buffer) Write(data []byte) (int, error) {
	n := copy(b.data[b.end:], data)
	b.end += int32(n)
	return n, nil
}

// Read reads bytes from the Buffer into the given slice.
func (b *Buffer) Read(data []byte) (int, error) {
	n := copy(data, b.data[b.start:b.end])
	b.start += int32(n)
	return n, nil
}

// ReadFullFrom reads exactly 'size' bytes from the reader into the Buffer.
func (b *Buffer) ReadFullFrom(reader io.Reader, size int32) (int, error) {
	if b.Cap() < size {
		return 0, errors.New("buffer capacity insufficient for ReadFullFrom")
	}
	n, err := io.ReadFull(reader, b.data[b.end:b.end+size])
	b.end += int32(n)
	return n, err
}

// ReadFrom reads from the reader until EOF or Buffer is full.
func (b *Buffer) ReadFrom(reader io.Reader) (int64, error) {
	n, err := reader.Read(b.data[b.end:])
	b.end += int32(n)
	return int64(n), err
}

// Advance advances the start pointer of the Buffer.
func (b *Buffer) Advance(size int32) {
	b.start += size
	if b.start > b.end {
		b.start = b.end // Prevent start from exceeding end
	}
}

// IsFull checks if the buffer is full.
func (b *Buffer) IsFull() bool {
	return b.Len() == b.Cap()
}


// MultiBuffer is a list of Buffers. The order of Buffer matters.
type MultiBuffer []*Buffer

// MergeMulti merges content from src to dest, and returns the new address of dest and src
func MergeMulti(dest MultiBuffer, src MultiBuffer) (MultiBuffer, MultiBuffer) {
	dest = append(dest, src...)
	for idx := range src {
		src[idx] = nil
	}
	return dest, src[:0]
}

// MergeBytes merges the given bytes into MultiBuffer and return the new address of the merged MultiBuffer.
func MergeBytes(dest MultiBuffer, src []byte) MultiBuffer {
	n := len(dest)
	if n > 0 && !(dest)[n-1].IsFull() {
		nBytes, _ := (dest)[n-1].Write(src)
		src = src[nBytes:]
	}

	for len(src) > 0 {
		b := New()
		nBytes, _ := b.Write(src)
		src = src[nBytes:]
		dest = append(dest, b)
	}

	return dest
}

// ReleaseMulti releases all content of the MultiBuffer, and returns an empty MultiBuffer.
func ReleaseMulti(mb MultiBuffer) MultiBuffer {
	for i := range mb {
		mb[i].Release()
		mb[i] = nil
	}
	return mb[:0]
}

// Copy copied the beginning part of the MultiBuffer into the given byte array.
func (mb MultiBuffer) Copy(b []byte) int {
	total := 0
	for _, bb := range mb {
		nBytes := copy(b[total:], bb.Bytes())
		total += nBytes
		if int32(nBytes) < bb.Len() {
			break
		}
	}
	return total
}

// ReadFrom reads all content from reader until EOF.
func ReadFrom(reader io.Reader) (MultiBuffer, error) {
	mb := make(MultiBuffer, 0, 16)
	for {
		b := New()
		_, err := b.ReadFullFrom(reader, Size)
		if b.IsEmpty() {
			b.Release()
		} else {
			mb = append(mb, b)
		}
		if err != nil {
			if errors.Cause(err) == io.EOF || errors.Cause(err) == io.ErrUnexpectedEOF {
				return mb, nil
			}
			return mb, err
		}
	}
}

// SplitBytes splits the given amount of bytes from the beginning of the MultiBuffer.
// It returns the new address of MultiBuffer leftover, and number of bytes written into the input byte slice.
func SplitBytes(mb MultiBuffer, b []byte) (MultiBuffer, int) {
	totalBytes := 0
	endIndex := -1
	for i := range mb {
		pBuffer := mb[i]
		nBytes, _ := pBuffer.Read(b)
		totalBytes += nBytes
		b = b[nBytes:]
		if !pBuffer.IsEmpty() {
			endIndex = i
			break
		}
		pBuffer.Release()
		mb[i] = nil
	}

	if endIndex == -1 {
		mb = mb[:0]
	} else {
		mb = mb[endIndex:]
	}

	return mb, totalBytes
}

// SplitFirstBytes splits the first buffer from MultiBuffer, and then copy its content into the given slice.
func SplitFirstBytes(mb MultiBuffer, p []byte) (MultiBuffer, int) {
	mb, b := SplitFirst(mb)
	if b == nil {
		return mb, 0
	}
	n := copy(p, b.Bytes())
	b.Release()
	return mb, n
}

// Compact returns another MultiBuffer by merging all content of the given one together.
func Compact(mb MultiBuffer) MultiBuffer {
	if len(mb) == 0 {
		return mb
	}

	mb2 := make(MultiBuffer, 0, len(mb))
	last := mb[0]

	for i := 1; i < len(mb); i++ {
		curr := mb[i]
		if last.Len()+curr.Len() > Size {
			mb2 = append(mb2, last)
			last = curr
		} else {
			// common.Must2(last.ReadFrom(curr)) // This common.Must2 is from mux package, not buf.
			// Assuming ReadFrom returns (int64, error), we need to handle it here.
			_, err := last.ReadFrom(curr)
			if err != nil {
				// Handle error appropriately, or panic if it's considered unrecoverable.
				panic(err) // For now, panic like common.Must2
			}
			curr.Release()
		}
	}

	mb2 = append(mb2, last)
	return mb2
}

// SplitFirst splits the first Buffer from the beginning of the MultiBuffer.
func SplitFirst(mb MultiBuffer) (MultiBuffer, *Buffer) {
	if len(mb) == 0 {
		return mb, nil
	}

	b := mb[0]
	mb[0] = nil
	mb = mb[1:]
	return mb, b
}

// SplitSize splits the beginning of the MultiBuffer into another one, for at most size bytes.
func SplitSize(mb MultiBuffer, size int32) (MultiBuffer, MultiBuffer) {
	if len(mb) == 0 {
		return mb, nil
	}

	if mb[0].Len() > size {
		b := New()
		copy(b.Extend(size), mb[0].BytesTo(size))
		mb[0].Advance(size)
		return mb, MultiBuffer{b}
	}

	totalBytes := int32(0)
	var r MultiBuffer
	endIndex := -1
	for i := range mb {
		if totalBytes+mb[i].Len() > size {
			endIndex = i
			break
		}
		totalBytes += mb[i].Len()
		r = append(r, mb[i])
		mb[i] = nil
	}
	if endIndex == -1 {
		// To reuse mb array
		mb = mb[:0]
	} else {
		mb = mb[endIndex:]
	}
	return mb, r
}

// WriteMultiBuffer writes all buffers from the MultiBuffer to the Writer one by one, and return error if any, with leftover MultiBuffer.
func WriteMultiBuffer(writer io.Writer, mb MultiBuffer) (MultiBuffer, error) {
	for {
		mb2, b := SplitFirst(mb)
		mb = mb2
		if b == nil {
			break
		}

		_, err := writer.Write(b.Bytes())
		b.Release()
		if err != nil {
			return mb, err
		}
	}

	return nil, nil
}

// Len returns the total number of bytes in the MultiBuffer.
func (mb MultiBuffer) Len() int32 {
	if mb == nil {
		return 0
	}

	size := int32(0)
	for _, b := range mb {
		size += b.Len()
	}
	return size
}

// IsEmpty returns true if the MultiBuffer has no content.
func (mb MultiBuffer) IsEmpty() bool {
	for _, b := range mb {
		if !b.IsEmpty() {
			return false
		}
	}
	return true
}

// String returns the content of the MultiBuffer in string.
func (mb MultiBuffer) String() string {
	v := make([]interface{}, len(mb))
	for i, b := range mb {
		v[i] = b
	}
	return serial.Concat(v...)
}

// MultiBufferContainer is a ReadWriteCloser wrapper over MultiBuffer.
type MultiBufferContainer struct {
	MultiBuffer
}

// Read implements io.Reader.
func (c *MultiBufferContainer) Read(b []byte) (int, error) {
	if c.MultiBuffer.IsEmpty() {
		return 0, io.EOF
	}

	mb, nBytes := SplitBytes(c.MultiBuffer, b)
	c.MultiBuffer = mb
	return nBytes, nil
}

// ReadMultiBuffer implements Reader.
func (c *MultiBufferContainer) ReadMultiBuffer() (MultiBuffer, error) {
	mb := c.MultiBuffer
	c.MultiBuffer = nil
	return mb, nil
}

// Write implements io.Writer.
func (c *MultiBufferContainer) Write(b []byte) (int, error) {
	c.MultiBuffer = MergeBytes(c.MultiBuffer, b)
	return len(b), nil
}

// WriteMultiBuffer implements Writer.
func (c *MultiBufferContainer) WriteMultiBuffer(b MultiBuffer) error {
	mb, _ := MergeMulti(c.MultiBuffer, b)
	c.MultiBuffer = mb
	return nil
}

// Close implements io.Closer.
func (c *MultiBufferContainer) Close() error {
	c.MultiBuffer = ReleaseMulti(c.MultiBuffer)
	return nil
}

// SizeCounter is a buf.CopyOption that counts the size of bytes being copied.
type SizeCounter struct {
	Size int64
}

// Apply implements buf.CopyOption.
func (sc *SizeCounter) Apply(b *Buffer) {
	atomic.AddInt64(&sc.Size, int64(b.Len()))
}

// CountSize creates a buf.CopyOption that counts the size of bytes being copied.
func CountSize(sc *SizeCounter) CopyOption {
	return func(b *Buffer) {
		sc.Apply(b)
	}
}

// Copy copies bytes from reader to writer.
// It stops when either reader returns EOF or writer returns error.
func Copy(reader Reader, writer Writer, opts ...CopyOption) error {
	for {
		mb, err := reader.ReadMultiBuffer()
		if err != nil {
			if errors.Cause(err) == io.EOF {
				return nil
			}
			return err
		}

		if mb.IsEmpty() {
			ReleaseMulti(mb) // Corrected: Call as function
			continue
		}

		for _, opt := range opts {
			for _, b := range mb {
				opt(b)
			}
		}

		if err := writer.WriteMultiBuffer(mb); err != nil {
			return err
		}
	}
}

// CopyOnceTimeout copies bytes from reader to writer once, with timeout.
func CopyOnceTimeout(reader Reader, writer Writer, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-timer.C:
		return ErrReadTimeout
	case result := <-readMultiBufferAsync(reader): // Receive the struct directly
		if result.Err != nil { // Access the named error field (Err)
			return result.Err
		}
		if result.MultiBuffer.IsEmpty() {
			ReleaseMulti(result.MultiBuffer) // Corrected: Call as function
			return ErrNotTimeoutReader
		}
		return writer.WriteMultiBuffer(result.MultiBuffer)
	}
}

// Reader is an interface for reading MultiBuffer.
type Reader interface {
	ReadMultiBuffer() (MultiBuffer, error)
}

// Writer is an interface for writing MultiBuffer.
type Writer interface {
	WriteMultiBuffer(MultiBuffer) error
}

// CopyOption is an option for Copy.
type CopyOption func(*Buffer)

// ErrReadTimeout is the error when read timeout.
var ErrReadTimeout = errors.New("read timeout")

// ErrNotTimeoutReader is the error when reader is not timeout reader.
var ErrNotTimeoutReader = errors.New("reader is not timeout reader")

type readResult struct {
	MultiBuffer
	Err error // Renamed the embedded error field to Err
}

func readMultiBufferAsync(reader Reader) chan readResult {
	ch := make(chan readResult, 1)
	go func() {
		mb, err := reader.ReadMultiBuffer()
		ch <- readResult{mb, err}
		close(ch)
	}()
	return ch
}

