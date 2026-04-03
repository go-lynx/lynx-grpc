# gRPC Plugin for Lynx Framework

This plugin provides both gRPC service (server) and client functionality for the Lynx framework, offering features such as TLS support, middleware integration, and configuration management.

## Version & Migration Notes

- Module path: `github.com/go-lynx/lynx-grpc`
- Runtime plugin names: `grpc.service` and `grpc.client`
- Local release audit note: the current [`go.mod`](./go.mod) still requires `github.com/go-lynx/lynx v1.6.0-beta`; this README documents the landed helper/config API, not a repository-wide stable dependency sweep.
- `GetGrpcServer(pluginManager)` and `GetGrpcClientConnection(serviceName, pluginManager)` now prefer an explicit `lynx.PluginManager`. Passing `nil` only works after the default Lynx app has already been initialized.
- Client-side legacy static `services` config remains supported for compatibility, but `subscribe_services` is the preferred configuration and the one new deployments should use.

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
go get github.com/go-lynx/lynx-grpc
```

## Configuration

### Configuration Fields Reference

The gRPC plugin configuration is delivered through protobuf with separate configurations for service (server) and client.

#### gRPC Service Configuration (`lynx.grpc.service`)

See `conf/service.proto` for field definitions:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `network` | `string` | `"tcp"` | Network type (e.g., `"tcp"`, `"unix"`) |
| `addr` | `string` | `":9090"` | Server listen address (e.g., `":9090"`, `"localhost:9090"`) |
| `tls_enable` | `bool` | `false` | Enable TLS/gRPCS encryption |
| `tls_auth_type` | `int32` | `0` | TLS client auth type (0–4, see below) |
| `timeout` | `Duration` | not set | Maximum duration for handling a single gRPC request |
| `max_concurrent_streams` | `uint32` | `0` (unlimited) | Max concurrent streams per HTTP/2 connection. Recommended: 100–500 (small), 500–2000 (medium), 2000–10000 (large) |
| `max_recv_msg_size` | `uint32` | `0` (~4 MB gRPC default) | Maximum inbound message size in bytes |
| `max_send_msg_size` | `uint32` | `0` (~4 MB gRPC default) | Maximum outbound message size in bytes |

**TLS Authentication Types:**
- `0`: No client authentication
- `1`: Request client certificate, but not mandatory
- `2`: Require client certificate
- `3`: Verify client certificate
- `4`: Require and verify client certificate

#### gRPC Client Configuration (`lynx.grpc.client`)

See `conf/client.proto` for field definitions:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `default_timeout` | `Duration` | `10s` | Default timeout for all outbound gRPC calls |
| `default_keep_alive` | `Duration` | `30s` | Keep-alive ping interval for idle connections |
| `max_retries` | `int32` | `3` | Maximum retries for failed requests |
| `retry_backoff` | `Duration` | `1s` | Backoff duration between retries |
| `max_connections` | `int32` | `10` | Maximum connections per target service |
| `tls_enable` | `bool` | `false` | Enable TLS for all client connections |
| `tls_auth_type` | `int32` | `0` | TLS client auth type (0–4) |
| `connection_pooling` | `bool` | `false` | Enable connection pooling |
| `pool_size` | `int32` | `5` | Connections in the pool per service |
| `idle_timeout` | `Duration` | not set | Idle connection eviction timeout |
| `health_check_enabled` | `bool` | `false` | Enable gRPC health check protocol per connection |
| `health_check_interval` | `Duration` | `30s` | How often to probe connection health |
| `metrics_enabled` | `bool` | `false` | Enable per-method Prometheus metrics on the client |
| `tracing_enabled` | `bool` | `false` | Inject trace context into outgoing metadata (requires lynx-tracer) |
| `logging_enabled` | `bool` | `false` | Log method, target, duration, and trace ID per call |
| `max_message_size` | `int32` | `0` (4 MB gRPC default) | Max inbound/outbound message size in bytes |
| `compression_enabled` | `bool` | `false` | Enable message compression |
| `compression_type` | `string` | `"gzip"` | Compression algorithm: `gzip`, `deflate` |
| `subscribe_services` | `repeated subscribe_service` | `[]` | Service discovery subscriptions (preferred) |
| `services` _(deprecated)_ | `repeated static_service` | `[]` | Legacy static service list; use `subscribe_services` |

#### Subscribe Service Configuration (`subscribe_services[*]`)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | `string` | — (required) | Service name for discovery and `GetConnection(name)` |
| `endpoint` | `string` | `""` | Static endpoint override (skips discovery when set) |
| `timeout` | `Duration` | inherits global | Per-service timeout override |
| `tls_enable` | `bool` | inherits global | Enable TLS for this service |
| `tls_auth_type` | `int32` | inherits global | TLS auth type for this service |
| `max_retries` | `int32` | inherits global | Per-service retry limit |
| `required` | `bool` | `false` | Fail startup if this service is unreachable |
| `metadata` | `map<string,string>` | `{}` | Arbitrary key-value metadata sent with requests |
| `load_balancer` | `string` | `"round_robin"` | LB strategy: `round_robin`, `random`, `weighted_round_robin` |
| `circuit_breaker_enabled` | `bool` | `false` | Enable circuit breaker for this service |
| `circuit_breaker_threshold` | `int32` | `5` | Failure count that trips the circuit breaker |

### Basic Configuration Example

```yaml
lynx:
  grpc:
    # gRPC Service Configuration (Server-side)
    service:
      network: "tcp"
      addr: ":9090"
      timeout: { seconds: 10 }
      tls_enable: true
      tls_auth_type: 4  # Mutual TLS authentication
      max_concurrent_streams: 1000

    # gRPC Client Configuration (Client-side)
    client:
      default_timeout: { seconds: 10 }
      default_keep_alive: { seconds: 30 }
      max_retries: 3
      retry_backoff: { seconds: 1 }
      max_connections: 10
      tls_enable: true
      tls_auth_type: 4
      connection_pooling: true
      pool_size: 5
      tracing_enabled: true
      logging_enabled: true
```

For discovery-first deployments, prefer `subscribe_services` over the deprecated static `services` list:

```yaml
lynx:
  grpc:
    client:
      default_timeout: { seconds: 10 }
      default_keep_alive: { seconds: 30 }
      max_retries: 3
      retry_backoff: { seconds: 1 }
      max_connections: 10
      tls_enable: true
      tls_auth_type: 4
      connection_pooling: true
      pool_size: 5
      health_check_enabled: true
      health_check_interval: { seconds: 30 }
      metrics_enabled: true
      tracing_enabled: true
      logging_enabled: true
      subscribe_services:
        - name: "user-service"
          required: true
          timeout: { seconds: 2 }
          metadata:
            tenant: "default"
          load_balancer: "round_robin"
          circuit_breaker_enabled: true
          circuit_breaker_threshold: 5
        - name: "order-service"
          endpoint: "127.0.0.1:9001"
          tls_enable: false
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
- `health_check_enabled`: Enable periodic health probing for pooled connections
- `health_check_interval`: Interval between health probes
- `metrics_enabled`: Enable client-side Prometheus metrics
- `tracing_enabled`: Enable trace context injection into gRPC metadata (recommended when using lynx-tracer)
- `logging_enabled`: Enable request/response logging on the client
- `max_message_size`: Max inbound/outbound message size in bytes
- `compression_enabled`: Enable gzip/deflate compression
- `compression_type`: Compression algorithm used when compression is enabled
- `subscribe_services`: Preferred discovery/static-override list used by `GetConnection(serviceName)`
- `services`: Service-specific configurations (deprecated; use `subscribe_services`)

## Usage

### Basic Usage

```go
package main

import (
    lynxapp "github.com/go-lynx/lynx"
    lynxgrpc "github.com/go-lynx/lynx-grpc"
    pb "your/protobuf/package"
)

func main() {
    // After the default Lynx application has been initialized, pass its plugin manager
    // explicitly. A nil pluginManager fallback only works after lynxapp.Lynx() is ready.
    server, err := lynxgrpc.GetGrpcServer(lynxapp.Lynx().GetPluginManager())
    if err != nil {
        log.Fatalf("Failed to get gRPC server: %v", err)
    }
    
    // Register your gRPC service
    pb.RegisterYourServiceServer(server, &YourServiceImpl{})
    
    // Start the application via your normal boot path
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
    lynxapp "github.com/go-lynx/lynx"
    lynxgrpc "github.com/go-lynx/lynx-grpc"
    "google.golang.org/grpc"
)

func main() {
    server, err := lynxgrpc.GetGrpcServer(lynxapp.Lynx().GetPluginManager())
    if err != nil {
        log.Fatalf("Failed to get gRPC server: %v", err)
    }
    
    // Add your custom middleware
    server.Use(YourCustomMiddleware())
    
    // Continue starting the application through your normal Lynx boot path.
}

func YourCustomMiddleware() grpc.UnaryServerInterceptor {
    return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
        // Your middleware logic here
        return handler(ctx, req)
    }
}
```

### Client Helper Usage

```go
package main

import (
    "context"

    lynxapp "github.com/go-lynx/lynx"
    lynxgrpc "github.com/go-lynx/lynx-grpc"
    pb "your/protobuf/package"
)

func main() {
    conn, err := lynxgrpc.GetGrpcClientConnection("user-service", lynxapp.Lynx().GetPluginManager())
    if err != nil {
        log.Fatalf("Failed to get gRPC client connection: %v", err)
    }

    client := pb.NewUserServiceClient(conn)
    _, err = client.GetUser(context.Background(), &pb.GetUserRequest{UserId: "123"})
    if err != nil {
        log.Printf("request failed: %v", err)
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
