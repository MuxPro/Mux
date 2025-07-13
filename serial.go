// 文件: serial/serial.go
package mux

import (
    "encoding/binary"
    "io"
)

// 写 uint16
func WriteUint16(w io.Writer, v uint16) error {
    return binary.Write(w, binary.BigEndian, v)
}

// 写 uint32
func WriteUint32(w io.Writer, v uint32) error {
    return binary.Write(w, binary.BigEndian, v)
}
