package mux

//go:generate go run github.com/v2fly/v2ray-core/v5/common/errors/errorgen

import "github.com/v2fly/v2ray-core/v5/common/net"

// Mux.Pro Address and Port (Updated from Mux.Cool)
var (
	// muxProAddress represents the target address for Mux.Pro traffic.
	muxProAddress = net.ParseAddress("mux.pro")
	// muxProPort represents the target port for Mux.Pro traffic.
	muxProPort = net.Port(9528)
)

// MuxProVersion represents the current Mux.Pro protocol version.
const MuxProVersion uint32 = 0x00000000 // MuxPro/0.0
