package mux

import (
    "encoding/binary"
    "io"
)

// Must panics if the given error is not nil.
// 这是 V2Ray 中常见的错误处理模式，用于简化代码。
// 当一个错误被认为是“绝不应该发生”的情况下，可以用它来包裹函数调用。
func Must(err error) {
	if err != nil {
		panic(err)
	}
}

// Must2 an alias for Must, but for functions that return two values,
// with the second one being an error. It returns the first value.
// 这个函数解决了你遇到的 (int, error) 的问题。
// 它接收一个值和一个 error，如果 error 不为 nil 就 panic，否则返回第一个值。
func Must2[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// 写 uint16
func WriteUint16(w io.Writer, v uint16) error {
    return binary.Write(w, binary.BigEndian, v)
}

// 写 uint32
func WriteUint32(w io.Writer, v uint32) error {
    return binary.Write(w, binary.BigEndian, v)
}

// WriteAll writes all bytes from a slice to a writer.
// It loops until all bytes are written or an error occurs.
// This is a manual implementation of what io.WriteFull does.
//
// w: The writer to write to (e.g., a network connection).
// b: The byte slice to write.
// Returns an error if the write operation fails.
func WriteAll(b []byte) error {
	// totalBytesToWrite 是我们需要写入的总字节数。
	totalBytesToWrite := len(b)
	
	// bytesWritten 是已经成功写入的字节数。
	bytesWritten := 0

	// 循环直到所有字节都已写入。
	for bytesWritten < totalBytesToWrite {
		// 从上次写入结束的位置开始，尝试写入剩余的字节。
		// b[bytesWritten:] 是一个切片，指向尚未写入的数据。
		n, err := w.Write(b[bytesWritten:])
		if err != nil {
			// 如果发生任何错误（例如连接断开），立即返回错误。
			return err
		}

		// 将本次写入的字节数累加到总数中。
		bytesWritten += n
	}

	// 如果循环正常结束，说明所有字节都已成功写入，返回 nil 错误。
	return nil
}

