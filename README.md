# gRPC Plugin for Lynx Framework

This plugin provides both gRPC service (server) and client functionality for the Lynx framework, offering features such as TLS support, middleware integration, and configuration management.

## Features

### gRPC Service (Server)
- Full gRPC server implementation
- TLS support with client authentication
- Built-in support:
  - Tracing (OpenTelemetry; extracts trace from metadata)
  - Request logging (method, duration, trace id)
  - Per-method Prometheus metrics (FullMethod)
  - Optional in-process rate limiting, in-flight unary limit, and server-side circuit breaker (see `lynx.grpc.service` options in PRODUCTION_READINESS.md)
  - Request validation (proto validate)
  - Panic recovery
- Dynamic configuration with validation
- Comprehensive health checking
- Configurable graceful shutdown
- Error handling and recovery

### gRPC Client
- Full gRPC client implementation
- Connection pooling and management (with deadlock-safe eviction)
- Unary interceptors applied per connection:
  - **Tracing**: injects trace context into outgoing metadata (when `tracing_enabled` is true; works with lynx-tracer)
  - **Metrics**: per-method and per-target request count and duration
  - **Circuit breaker**: wraps each RPC so failures are counted and the circuit opens when configured threshold is reached
  - **Logging**: method, target, duration, and optional trace id
- Service discovery integration
- TLS support with client authentication
- `GetConnection(serviceName)` prefers `subscribe_services` config for that name, then falls back to global config + discovery
- Load balancing and failover

## Installation

```bash
go get github.com/go-lynx/plugin-grpc/v2
```

## Configuration

The plugin can be configured through the Lynx configuration system with separate configurations for service and client:

```yaml
lynx:
  grpc:
    # gRPC Service Configuration (Server-side)
    service:
      network: "tcp"
      addr: ":9090"
      timeout: 10
      tls_enable: true
      tls_auth_type: 4  # Mutual TLS authentication
    
    # gRPC Client Configuration (Client-side)
    client:
      default_timeout: "10s"
      default_keep_alive: "30s"
      max_retries: 3
      retry_backoff: "1s"
      max_connections: 10
      tls_enable: true
      tls_auth_type: 4
      connection_pooling: true
      pool_size: 5
```

### Configuration Options

#### gRPC Service Configuration (`lynx.grpc.service`)

- `network`: Network type (default: "tcp")
- `addr`: Server address (default: ":9090")
- `timeout`: Request timeout duration (in seconds)
- `tls_enable`: Enable/disable TLS
- `tls_auth_type`: TLS authentication type
  - 0: No client authentication
  - 1: Request client certificate
  - 2: Require any client certificate
  - 3: Verify client certificate if given
  - 4: Require and verify client certificate
- `max_concurrent_streams`: Maximum number of concurrent streams per HTTP/2 connection (default: 1000)
  - **Important**: This parameter controls server resource usage and prevents overload
  - **Recommended values**:
    - Small service: 100-500
    - Medium service: 500-2000
    - Large service: 2000-10000
  - Setting this too high may cause resource exhaustion
  - Setting this too low may unnecessarily limit concurrent requests
- `max_recv_msg_size`: Maximum inbound message size (bytes). Default 0 uses the gRPC server default (~4MB). Set explicit values to protect from oversized payloads.
- `max_send_msg_size`: Maximum outbound message size (bytes). Default 0 uses the gRPC server default (~4MB). Set explicit values to keep responses within expected limits.

#### gRPC Client Configuration (`lynx.grpc.client`)

- `default_timeout`: Default timeout for gRPC client requests
- `default_keep_alive`: Default keep-alive interval for gRPC connections
- `max_retries`: Maximum number of retries for failed requests
- `retry_backoff`: Backoff duration between retries
- `max_connections`: Maximum number of connections per service
- `tls_enable`: Enable TLS for gRPC client connections
- `tls_auth_type`: TLS authentication type (0-4)
- `connection_pooling`: Enable connection pooling
- `pool_size`: Connection pool size
- `tracing_enabled`: Enable trace context injection into gRPC metadata (recommended when using lynx-tracer)
- `logging_enabled`: Enable request/response logging on the client
- `services`: Service-specific configurations (deprecated; use `subscribe_services`)

## Usage

### Basic Usage

```go
package main

import (
    "github.com/go-lynx/lynx/app"
    "github.com/go-lynx/plugin-grpc/v2"
    pb "your/protobuf/package"
)

func main() {
    // Initialize your Lynx application
    application := app.NewApplication()
    
    // The gRPC plugin will be automatically registered and initialized
    
    // Get the gRPC server instance
    server, err := grpc.GetGrpcServer()
    if err != nil {
        log.Fatalf("Failed to get gRPC server: %v", err)
    }
    
    // Register your gRPC service
    pb.RegisterYourServiceServer(server, &YourServiceImpl{})
    
    // Start the application
    if err := application.Run(); err != nil {
        panic(err)
    }
}
```

### With TLS

To use TLS, you need to:

1. Enable TLS in configuration
2. Provide certificates through the Lynx certificate management system
3. Configure client authentication type if needed

```go
// Your certificates will be automatically loaded from the configuration
// and applied to the gRPC server
```

### Custom Middleware

The plugin comes with several built-in middleware options. You can also add your own middleware:

```go
package main

import (
    "context"
    "github.com/go-lynx/lynx/app"
    "github.com/go-lynx/plugin-grpc/v2"
    "google.golang.org/grpc"
)

func main() {
    // Initialize your application
    application := app.NewApplication()
    
    // Get the gRPC server
    server, err := grpc.GetGrpcServer()
    if err != nil {
        log.Fatalf("Failed to get gRPC server: %v", err)
    }
    
    // Add your custom middleware
    server.Use(YourCustomMiddleware())
    
    // Start the application
    if err := application.Run(); err != nil {
        panic(err)
    }
}

func YourCustomMiddleware() grpc.UnaryServerInterceptor {
    return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
        // Your middleware logic here
        return handler(ctx, req)
    }
}
```

## Health Checking

The plugin implements comprehensive health checking through the Lynx plugin system. Health checks include:

- Server initialization status
- Configuration validation
- Port availability
- TLS configuration validation (if enabled)

You can monitor the gRPC server's health status through your application's health checking mechanism.

## Monitoring and Metrics

The plugin provides Prometheus metrics for monitoring:

- `lynx_grpc_server_up`: Whether the gRPC server is up (labels: server_name, address)
- `lynx_grpc_requests_total`: Total number of gRPC requests (labels: method, status; method is the full RPC method name)
- `lynx_grpc_request_duration_seconds`: Duration of gRPC requests (labels: method)
- `lynx_grpc_active_connections`: Number of active gRPC connections (labels: server_name)
- `lynx_grpc_server_start_time_seconds`: Unix timestamp of server start time
- `lynx_grpc_server_errors_total`: Total number of server errors (labels: error_type)

Server metrics and request logging are implemented via native gRPC UnaryServerInterceptors so that the full method name and trace id (from context) are available. Client metrics and tracing are implemented via UnaryClientInterceptors; enable `tracing_enabled` in client config to propagate trace context to the server (works with lynx-tracer).

## Dependencies

- github.com/go-kratos/kratos/v2
- github.com/go-lynx/lynx
- google.golang.org/grpc

## License

Apache License 2.0
