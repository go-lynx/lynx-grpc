# lynx-grpc Production Readiness

This document describes optional configuration, recommended practices, and known limitations of the gRPC plugin in production.

## Server Optional Configuration (lynx.grpc.service)

In addition to proto-defined options (`network`, `addr`, `timeout`, `tls_enable`, `tls_auth_type`, `max_concurrent_streams`, `max_recv_msg_size`, `max_send_msg_size`), the following options can be extended under the same config prefix via YAML/JSON:

### Graceful Shutdown

- **graceful_shutdown_timeout** (duration): Maximum time to wait for in-flight requests to drain when stopping the gRPC server. Default: `30s`.

### Middleware Toggles

- **enable_tracing** (bool): Whether to enable Kratos tracing middleware. Default: `true`.
- **enable_request_logging** (bool): Whether to enable request logging interceptor. Default: `true`.
- **enable_metrics** (bool): Whether to enable Prometheus metrics interceptor. Default: `true`.

### Rate Limiting (In-Process)

- **rate_limit** (object):
  - **enabled** (bool): Whether rate limiting is enabled.
  - **rate_per_second** (float): Allowed requests per second.
  - **burst** (int): Burst capacity; recommended ≥ rate_per_second + 1.

### Concurrency Control

- **max_inflight_unary** (int32): Maximum concurrent Unary RPCs; `0` means unlimited. Default: `0`. Use to avoid single-node overload.

### Server-Side Circuit Breaker

- **circuit_breaker** (object):
  - **enabled** (bool): Whether server-side circuit breaker is enabled.
  - **failure_threshold** (int): Number of failures before opening the circuit. Default: `5`.
  - **recovery_timeout** (duration): Time after opening before entering half-open. Default: `30s`.
  - **success_threshold** (int): Consecutive successes in half-open required to close the circuit. Default: `3`.
  - **timeout** (duration): Per-request timeout. Default: `10s`.
  - **max_concurrent_requests** (int): Max concurrent requests allowed in half-open. Default: `10`.

### Configuration Example

```yaml
lynx:
  grpc:
    service:
      network: "tcp"
      addr: ":9090"
      timeout: 10
      tls_enable: true
      tls_auth_type: 4
      max_concurrent_streams: 1000
      max_recv_msg_size: 10485760
      max_send_msg_size: 10485760
      # Extended options
      graceful_shutdown_timeout: "30s"
      enable_tracing: true
      enable_request_logging: true
      enable_metrics: true
      max_inflight_unary: 500
      rate_limit:
        enabled: true
        rate_per_second: 1000
        burst: 1100
      circuit_breaker:
        enabled: true
        failure_threshold: 5
        recovery_timeout: "30s"
        success_threshold: 3
        timeout: "10s"
        max_concurrent_requests: 10
```

## Client Usage

- **Service discovery**: The gRPC client now first tries to resolve discovery from shared runtime resources such as `polaris.control.plane.service_discovery` and `nacos.control.plane.service_discovery`, then falls back to the default Lynx app's `GetServiceDiscovery()`. `grpc.SetDiscovery(discovery)` is still available as an explicit override when needed.
- **Obtaining connections**: Obtain connections via the registered gRPC client plugin instance (e.g., get the `grpc.client` plugin from Lynx’s PluginManager, then call `GetConnection(serviceName)`). Avoid using convenience functions that create new plugin instances, which can return uninitialized config and connection pools.
- **Load balancer**: When `load_balancer` is configured for a service in `subscribe_services` (e.g. `round_robin`, `random`, `p2c`, `consistent_hash`), new connections use the LoadBalancer: each time a new connection is created from the pool, `SelectNode` is called to pick an instance and connect directly (connection-level load balancing). Without `load_balancer`, `discovery:///service_name` is used and gRPC handles resolution and default policy.
- **NodeFilter**: Node filtering via `ClientConfig.NodeFilter` is applied in `LoadBalancer.SelectNode` (filter instances, then apply policy). It only takes effect when using the LoadBalancer.
- **Lifecycle**: `grpc.client` and `grpc.service` both declare context-aware lifecycle now. Startup and shutdown can be driven by Lynx lifecycle contexts, and required-upstream checks honor cancellation.
- **Configuration updates**: Validation is supported in-process, but connection/server-affecting changes still apply on the next managed restart rather than live mutation of existing transports.

## Production Readiness Checklist

Confirm each item before going live to avoid production issues from missing configuration.

### Required

- [ ] **Connections must be obtained via PluginManager**  
  When obtaining gRPC connections in application code, use `grpc.GetGrpcClientConnection(serviceName, application.GetPluginManager())` or first `GetGrpcClientPlugin(pluginManager)` then `GetConnection(serviceName)`.  
  **Do not** use `GetOrCreateGrpcClientPlugin()` as the primary path in application code; it may return an uninitialized plugin (empty config, connection pool not started) and connection behavior will be undefined.

- [ ] **Confirm Discovery source when using service discovery**  
  Prefer publishing service discovery through the control plane plugin (`polaris` / `nacos`) or the default Lynx app so `grpc.client` can resolve it automatically.  
  If your deployment does not expose discovery that way, call `grpc.SetDiscovery(discovery)` explicitly during startup before the first gRPC client call.

- [ ] **TLS certificates provided by Lynx**  
  Client TLS uses certificates from `lynx.Lynx().Certificate()`. In production with TLS enabled, ensure Lynx is initialized and certificates are configured; otherwise TLS connection setup may panic.  
  Confirm that Lynx and certificate loading run before the first use of the gRPC client plugin.

### Recommended

- [ ] **Strict client config validation (optional)**  
  For critical services, call `ConfigValidator.ValidateClientConfig(config)` at startup or before config hot-reload to catch configuration errors early.

- [ ] **Monitoring and alerting**  
  Use Prometheus metrics exposed by the plugin (request volume, latency, circuit breaker state, connection count, etc.) for dashboards and alerts; set rules for “required service unavailable”, “connection failure rate spike”, and similar.

- [ ] **Canary and rollback**  
  For first rollout or major upgrades, use small traffic or single-instance canary and watch error rate, latency, and connection count; keep a fast rollback path (config or version rollback).

### Checklist Example

| Check item | Done |
|------------|------|
| Connections obtained via PluginManager | ☐ |
| Discovery source verified (auto or explicit SetDiscovery) | ☐ |
| Lynx and certificates correctly initialized for TLS | ☐ |
| Monitoring and alerting configured | ☐ |
| Canary/rollback plan ready | ☐ |

## Known Limitations and Notes

1. **Server rate limit source**: External rate limits from the control plane are not wired in; only the configured `rate_limit` is effective.
2. **Health check port**: Runtime `CheckHealth()` treats an unreachable listening port as a hard failure and returns an error immediately. During rollout, wire alerts to this failure instead of assuming the plugin will only warn.
3. **Config validation**: Besides the built-in `validateConfiguration()`, the client provides `ConfigValidator.ValidateClientConfig()` for strict validation before startup or config hot-reload; integrate as needed.

## Version and Compatibility

- Plugin version: v1.5.5 (aligned with grpc.service / grpc.client code).
- Lynx framework and Kratos versions are in `go.mod`; confirm compatibility with the current Lynx main version before production deployment.
