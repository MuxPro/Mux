# Mux.Pro Protocol Implementation

This repository contains a Go language implementation of the Mux.Pro protocol, an advanced multiplexing transport protocol designed for efficient data stream management over a single underlying reliable connection. It extends the concepts of Mux.Cool with enhanced sub-connection handling, explicit version negotiation, and credit-based flow control.

## 1\. Introduction

The Mux.Pro protocol aims to optimize network resource utilization by multiplexing multiple logical "sub-connections" over a single "main connection." This reduces the overhead associated with establishing numerous individual transport connections for various application data streams.

This implementation provides the core logic for both client and server sides of the Mux.Pro protocol, designed to operate on top of an established, reliable, ordered, and bidirectional data stream (e.g., TCP, QUIC stream).

**Note:** Mux.Pro is purely a multiplexing protocol and does not inherently provide cryptographic security, authentication, or anti-censorship capabilities. These aspects should be handled by the underlying transport layer (e.g., TLS) or by layering Mux.Pro with dedicated security protocols.

## 2\. Features

  * **Version Negotiation (MuxPro/0.0)**: Supports explicit version negotiation between client and server upon main connection establishment, ensuring protocol compatibility.
  * **Sub-connection Management**: Efficiently handles the lifecycle of multiple logical sub-connections over a single main connection, including creation, data transfer, and graceful termination.
  * **Credit-Based Flow Control**: Implements a credit-based flow control mechanism per sub-connection, preventing senders from overwhelming receivers and ensuring fair resource allocation.
  * **Error Handling**: Incorporates specific error codes for sub-connection termination reasons, providing clearer diagnostics.
  * **Protocol-Agnostic Underlying Transport**: Designed to work over any reliable stream-oriented transport.

## 3\. Project Structure

The implementation is organized into several Go files, each responsible for a specific aspect of the Mux.Pro protocol:

  * **`client.go`**: Implements the client-side logic for Mux.Pro, including initiating main connections, dispatching sub-connections, and handling inbound Mux.Pro frames from the server.
  * **`server.go`**: Implements the server-side logic for Mux.Pro, including accepting main connections, handling client-initiated sub-connection requests, and managing outbound Mux.Pro frames.
  * **`frame.go`**: Defines the Mux.Pro frame structure, metadata, session statuses, options, and error codes. It includes methods for marshaling and unmarshaling frame metadata.
  * **`session.go`**: Manages individual Mux.Pro sub-connections (`Session` objects) and their associated state, including credit for flow control.
  * **`writer.go`**: Provides a `Writer` abstraction for sending Mux.Pro frames over the underlying transport, incorporating the credit-based flow control logic.
  * **`reader.go`**: Provides a `PacketReader` and `StreamReader` for reading Mux.Pro frame data from the underlying transport.
  * **`common.go`**: Contains utility functions and common definitions used across the `mux` package.
  * **`errors.generated.go`**: Defines a custom error constructor for consistent error reporting within the `mux` package.

## 4\. How to Use

This `mux` package is designed to be integrated into a larger network proxy or application that provides an underlying reliable stream. Below are conceptual examples of how to initialize and use the client and server components.

### 4.1. Special Target Address

For compatibility and transparent integration within a proxy system (e.g., similar to V2Ray's routing), the Mux.Pro protocol suggests using `"mux.pro"` as a special target domain address. When a connection's target matches this address, it should be treated as a Mux.Pro main connection.

```go
// In client.go and server.go, the special address is defined as:
var (
	muxProAddress = net.DomainAddress("mux.pro")
	muxProPort    = net.Port(9527)
)
```

### 4.2. Client-Side Usage

To use the Mux.Pro client, you would typically:

1.  **Create a `DialingWorkerFactory`**: This factory is responsible for establishing the underlying main connection.
2.  **Create an `IncrementalWorkerPicker`**: This picker manages a pool of `ClientWorker` instances, creating new ones as needed.
3.  **Dispatch Sub-connections**: Use the `ClientManager` to dispatch new application links (sub-connections) to available Mux.Pro workers.

<!-- end list -->

```go
// Example (conceptual, requires external dependencies like V2Ray core for full context)
package main

import (
	"context"
	"log"
	"time"

	"github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/transport"
	"github.com/v2fly/v2ray-core/v5/transport/internet"
	"github.com/v2fly/v2ray-core/v5/transport/pipe"
	"your_project_path/mux" // Assuming your mux package is here
	"your_project_path/proxy" // Replace with your actual proxy interface
)

func main() {
	ctx := context.Background()

	// --- 1. Setup Mock Proxy and Dialer (replace with actual implementations) ---
	// In a real V2Ray setup, these would come from core.RequireFeatures
	mockOutboundProxy := &MockOutboundProxy{} // Implement proxy.Outbound
	mockDialer := &MockDialer{}               // Implement internet.Dialer

	// --- 2. Create Client Strategy ---
	clientStrategy := mux.ClientStrategy{
		MaxConcurrency: 8, // Max concurrent sub-connections per main connection
		MaxConnection:  16, // Max total connections (including closed) for cleanup
	}

	// --- 3. Create DialingWorkerFactory ---
	factory := mux.NewDialingWorkerFactory(ctx, mockOutboundProxy, mockDialer, clientStrategy)

	// --- 4. Create IncrementalWorkerPicker ---
	picker := &mux.IncrementalWorkerPicker{
		Factory: factory,
	}

	// --- 5. Create ClientManager ---
	clientManager := &mux.ClientManager{
		Enabled: true,
		Picker:  picker,
	}

	// --- 6. Dispatch a new Sub-connection ---
	// Simulate an incoming application connection (e.g., from an inbound handler)
	uplinkReader, uplinkWriter := pipe.New(pipe.WithSizeLimit(1024))
	downlinkReader, downlinkWriter := pipe.New(pipe.WithSizeLimit(1024))

	appLink := &transport.Link{
		Reader: uplinkReader,
		Writer: downlinkWriter,
	}

	// Context with target destination for the sub-connection
	subCtx := context.WithValue(ctx, "outbound_target", net.TCPDestination(net.ParseAddress("example.com"), 80))

	if clientManager.Enabled {
		err := clientManager.Dispatch(subCtx, appLink)
		if err != nil {
			log.Printf("Failed to dispatch sub-connection: %v", err)
			return
		}
		log.Println("Sub-connection dispatched successfully.")

		// Simulate data transfer (e.g., write to uplinkWriter, read from downlinkReader)
		go func() {
			uplinkWriter.Write([]byte("Hello from client sub-connection!"))
			time.Sleep(time.Second)
			uplinkWriter.Close()
		}()

		go func() {
			data, _ := downlinkReader.ReadMultiBuffer()
			log.Printf("Received from server sub-connection: %s", data.String())
			data.Release()
		}()

	} else {
		log.Println("Mux.Pro client is disabled.")
	}

	// Keep main goroutine alive for a while to observe logs
	time.Sleep(5 * time.Second)
}

// MockOutboundProxy and MockDialer would be actual implementations in a real V2Ray setup.
// For demonstration purposes, they can be simple structs satisfying the interfaces.
type MockOutboundProxy struct{}
func (m *MockOutboundProxy) Process(ctx context.Context, link *transport.Link, dialer internet.Dialer) error {
    log.Println("MockOutboundProxy: Processing connection...")
    // Simulate data flow through the underlying connection
    go func() {
        buf.Copy(link.Reader, link.Writer)
        link.Writer.Close()
    }()
    return nil
}
type MockDialer struct{}
func (m *MockDialer) Dial(ctx context.Context, dest net.Destination) (internet.Connection, error) {
    log.Printf("MockDialer: Dialing to %s...", dest.String())
    // Simulate a successful connection
    return &MockConnection{}, nil
}
type MockConnection struct{}
func (m *MockConnection) Read(b []byte) (n int, err error) { /* ... */ return 0, io.EOF }
func (m *MockConnection) Write(b []byte) (n int, err error) { /* ... */ return len(b), nil }
func (m *MockConnection) Close() error { return nil }
func (m *MockConnection) LocalAddr() net.Addr { return nil }
func (m *MockConnection) RemoteAddr() net.Addr { return nil }
func (m *MockConnection) SetDeadline(t time.Time) error { return nil }
func (m *MockConnection) SetReadDeadline(t time.Time) error { return nil }
func (m *MockConnection) SetWriteDeadline(t time.Time) error { return nil }
```

### 4.3. Server-Side Usage

To use the Mux.Pro server, you would typically:

1.  **Create a `Server` instance**: This instance will handle incoming Mux.Pro main connections.
2.  **Integrate with an Inbound Handler**: The `Server`'s `Dispatch` method should be called by your underlying transport's inbound handler when a connection targets the special "mux.pro" address.

<!-- end list -->

```go
// Example (conceptual, requires external dependencies like V2Ray core for full context)
package main

import (
	"context"
	"log"
	"time"

	core "github.com/v2fly/v2ray-core/v5"
	"github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/features/routing"
	"github.com/v2fly/v2ray-core/v5/transport"
	"github.com/v2fly/v2ray-core/v5/transport/pipe"
	"your_project_path/mux" // Assuming your mux package is here
)

func main() {
	ctx := context.Background()

	// --- 1. Setup Mock Dispatcher (replace with actual implementation) ---
	// In a real V2Ray setup, this would come from core.RequireFeatures
	mockDispatcher := &MockDispatcher{} // Implement routing.Dispatcher

	// --- 2. Create Mux.Pro Server Instance ---
	muxServer := mux.NewServer(core.ContextWith
		(ctx, func(d routing.Dispatcher) { mockDispatcher = d })) // Pass dispatcher via context

	// --- 3. Simulate an incoming Main Connection (e.g., from a TCP listener) ---
	// This link represents the raw underlying connection.
	uplinkReader, uplinkWriter := pipe.New(pipe.WithSizeLimit(1024))
	downlinkReader, downlinkWriter := pipe.New(pipe.WithSizeLimit(1024))

	// The Mux.Pro server's Dispatch method expects the *link* of the underlying connection
	// and the *destination* that triggered this connection (which should be "mux.pro").
	// It returns a link for the application-level data stream.
	appLink, err := muxServer.Dispatch(ctx, net.TCPDestination(net.DomainAddress("mux.pro"), 9527))
	if err != nil {
		log.Fatalf("Failed to dispatch main connection to Mux.Pro server: %v", err)
	}
	log.Println("Mux.Pro server successfully dispatched main connection.")

	// Simulate data flow into the server's Mux.Pro main connection
	// (e.g., from the client's underlying connection writer)
	go func() {
		uplinkWriter.Write([]byte("Initial data from client to server main connection"))
		time.Sleep(time.Second)
		uplinkWriter.Close()
	}()

	// Simulate application handling of the sub-connection data
	go func() {
		data, _ := appLink.Reader.ReadMultiBuffer()
		log.Printf("Server received application data from sub-connection: %s", data.String())
		data.Release()
		// Simulate sending response back
		appLink.Writer.Write([]byte("Response from server application"))
		appLink.Writer.Close()
	}()

	// Keep main goroutine alive for a while to observe logs
	time.Sleep(5 * time.Second)
}

// MockDispatcher would be an actual implementation in a real V2Ray setup.
type MockDispatcher struct{}
func (m *MockDispatcher) Dispatch(ctx context.Context, dest net.Destination) (*transport.Link, error) {
    log.Printf("MockDispatcher: Dispatching to application layer for %s...", dest.String())
    // Simulate an application-level pipe
    reader, writer := pipe.New(pipe.WithSizeLimit(1024))
    return &transport.Link{Reader: reader, Writer: writer}, nil
}
func (m *MockDispatcher) Type() interface{} { return "MockDispatcher" }
```

## 5\. Configuration (Conceptual)

In a complete system like V2Ray, Mux.Pro would be configured within the inbound and outbound proxy settings. For example, an outbound proxy might have a `mux` option:

```json
{
  "outbounds": [
    {
      "protocol": "vmess",
      "settings": { /* ... */ },
      "mux": {
        "enabled": true,
        "concurrency": 8 // Corresponds to ClientStrategy.MaxConcurrency
      }
    }
  ]
}
```

The `mux.ClientManager` and `mux.Server` would then be initialized based on these configuration parameters.

## 6\. Flow Control Details

The Mux.Pro implementation includes a credit-based flow control mechanism to prevent a fast sender from overwhelming a slow receiver.

  * Each `Session` (sub-connection) maintains a `sendCredit` counter.
  * When a sender wants to transmit data, it must first `ConsumeCredit`. If insufficient credit is available, the sender will block until more credit is granted.
  * Receivers, upon processing data, periodically send `CreditUpdate` frames back to the sender. These frames grant additional `sendCredit` to the sender, allowing it to resume transmission.
  * The `DefaultInitialCredit` (64KB) and `CreditUpdateThreshold` (32KB) constants in `session.go` define the initial credit and when a receiver should send a credit update, respectively.

## 7\. Error Handling and Logging

The implementation uses `newError` for consistent error creation and `WriteToLog` for logging errors and important events. This allows for centralized error management and integration with a larger logging framework.

## 8\. License

This project is licensed under the MIT License. See the LICENSE file for details.

## 9\. Credits

This implementation is based on the Mux.Pro Protocol Specification.

**Author:** v2eth
**Contact:** admin@wikiped.me
