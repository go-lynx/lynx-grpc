# lynx-grpc 生产可用性说明

本文档描述 gRPC 插件在生产环境中的可选配置、推荐实践与已知限制。

## 服务端可选配置（lynx.grpc.service）

除 proto 定义的 `network`、`addr`、`timeout`、`tls_enable`、`tls_auth_type`、`max_concurrent_streams`、`max_recv_msg_size`、`max_send_msg_size` 外，可通过同一配置前缀以 YAML/JSON 扩展以下选项：

### 优雅关闭

- **graceful_shutdown_timeout** (duration)：停止 gRPC 服务时等待请求排空的最长时间，默认 `30s`。

### 中间件开关

- **enable_tracing** (bool)：是否启用 Kratos 链路追踪中间件，默认 `true`。
- **enable_request_logging** (bool)：是否启用请求日志拦截器，默认 `true`。
- **enable_metrics** (bool)：是否启用 Prometheus 指标拦截器，默认 `true`。

### 限流（进程内）

- **rate_limit** (object)：
  - **enabled** (bool)：是否启用限流。
  - **rate_per_second** (float)：每秒允许的请求数。
  - **burst** (int)：突发容量，建议 ≥ rate_per_second + 1。

### 并发控制

- **max_inflight_unary** (int32)：同时进行的 Unary RPC 上限，0 表示不限制，默认 `0`。可用于防止单机过载。

### 服务端熔断

- **circuit_breaker** (object)：
  - **enabled** (bool)：是否启用服务端熔断。
  - **failure_threshold** (int)：失败次数达到该值后熔断打开，默认 `5`。
  - **recovery_timeout** (duration)：熔断打开后多久进入半开，默认 `30s`。
  - **success_threshold** (int)：半开状态下连续成功次数达到该值后关闭熔断，默认 `3`。
  - **timeout** (duration)：单次请求超时，默认 `10s`。
  - **max_concurrent_requests** (int)：半开状态允许的最大并发请求数，默认 `10`。

### 配置示例

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
      # 扩展选项
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

## 客户端使用说明

- **服务发现**：若使用 Polaris 等注册中心，需在应用侧调用 `grpc.SetDiscovery(discovery)` 注入，否则仅能使用静态 `endpoint` 或 `subscribe_services` 中的静态端点。
- **连接获取**：应通过已注册的 gRPC 客户端插件实例获取连接（例如通过 Lynx 的 PluginManager 获取 `grpc.client` 插件后再调用 `GetConnection(serviceName)`），避免使用会创建新插件实例的便捷函数导致拿到未初始化的配置与连接池。

## 生产上线检查清单

上线前请逐项确认，避免因漏配导致生产故障。

### 必须项

- [ ] **连接必须通过 PluginManager 获取**  
  业务代码中获取 gRPC 连接时，使用 `grpc.GetGrpcClientConnection(serviceName, application.GetPluginManager())` 或先 `GetGrpcClientPlugin(pluginManager)` 再 `GetConnection(serviceName)`。  
  **不要**在业务中直接使用 `GetOrCreateGrpcClientPlugin()` 作为主路径，否则可能拿到未初始化的插件实例（空配置、未启动连接池），连接行为不可预期。

- [ ] **使用服务发现时必须注入 Discovery**  
  若使用 Polaris 等注册中心，应用在启动阶段（在首次调用 gRPC 客户端前）必须调用 `grpc.SetDiscovery(discovery)` 注入。  
  否则 `discovery` 为 nil，只能使用静态 `endpoint`；若配置中仅写了服务名、未写 endpoint，会报错或无法建连。  
  建议在部署/启动脚本或初始化代码中显式调用，并在上线检查表中勾选确认。

- [ ] **TLS 证书由 Lynx 提供**  
  客户端 TLS 通过 `lynx.Lynx().Certificate()` 获取证书。生产环境若启用 TLS，须保证 Lynx 已正确初始化并配置好证书；否则在需要 TLS 建连时会 panic。  
  确认应用启动顺序中 Lynx 与证书加载先于 gRPC 客户端插件的首次使用。

### 推荐项

- [ ] **客户端配置严格校验（可选）**  
  对关键服务可在启动或配置热更前调用 `ConfigValidator.ValidateClientConfig(config)` 做一次严格校验，及早发现配置错误。

- [ ] **监控与告警**  
  使用插件暴露的 Prometheus 指标（请求量、时延、熔断状态、连接数等）配置监控大盘与告警；对「required 服务不可用」「连接失败率突增」等设置告警规则。

- [ ] **灰度与回滚**  
  首次上线或大版本升级时建议先小流量/单实例灰度，观察错误率、延迟、连接数；保留快速回滚方案（配置或版本回滚）。

### 检查表示例

| 检查项                         | 确认 |
|--------------------------------|------|
| 连接获取使用 PluginManager 传入 | ☐    |
| 使用注册中心时已调用 SetDiscovery | ☐    |
| TLS 场景下 Lynx 与证书已正确初始化 | ☐    |
| 监控与告警已配置                 | ☐    |
| 灰度/回滚方案已就绪              | ☐    |

## 已知限制与注意事项

1. **服务端限流来源**：当前服务端从 control plane 获取的“外部限流”未接入，仅配置中的 `rate_limit` 生效。
2. **健康检查端口**：服务端健康检查在“端口尚未监听”（如启动阶段）时对端口不可达仅打警告、不判失败，以避免误判；若长期端口不可达，需依赖监控与告警。
3. **配置校验**：客户端除内置 `validateConfiguration()` 外，还提供 `ConfigValidator.ValidateClientConfig()`，可用于启动前或配置热更前的严格校验；按需集成。

## 版本与兼容

- 插件版本：v2.0.0
- 依赖 Lynx 框架与 Kratos 的版本见 `go.mod`；生产部署前请确认与当前 Lynx 主版本兼容。
