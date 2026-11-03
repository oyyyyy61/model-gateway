# 代码走读注释存档

本文档保存 `8fea098`（`docs: annotate gateway code with Chinese walkthrough comments`）
中加入的中文走读注释原文，并标注它们在重构后代码中的对应位置。

## 背景

- `8fea098` 在 8 个文件中新增了 97 行中文注释。
- 随后的 `f522899` 重写了其中 7 个文件的实现：鉴权从单 Key 改为多 Key，
  handler/proxy/routing/main 的流程都发生了变化。
- `internal/middleware/middleware.go` 的 3 条注释在重构中保留了下来；其余 7 个
  文件因整体重写，原注释没有逐行保留。
- 当前代码已按新实现补回中文注释；本文档保留原始文字，避免再次丢失。

原文中的个别笔误（如“创造建”“不在接收”）按原样保留，便于和提交历史对照。

## 如何取回原文

```bash
git show 8fea098
git diff 8fea098 f522899 -- internal/gateway/handler.go
git checkout 8fea098 -- internal/gateway/handler.go   # 仅用于查看旧实现，不要直接提交
```

## 1. cmd/gateway/main.go

### 原始注释

```go
// API key鉴权
// 配置读取
// HTTP handler和代理
// 中间件负载均衡
// 模型实例选择、负载均衡策略
// 读取环境变量配置，返回Config struct，路径声明在internal\config\config.go
// 创建鉴权服务，读取配置
// 创建模型实例列表
// 创建代理服务，传入模型实例列表、HTTP客户端、请求超时时间和日志记录器
// 创建HTTP handler，传入代理服务、鉴权服务和日志记录器
// 创建HTTP服务器，传入监听地址、HTTP handler、读取头超时时间和空闲超时时间
// 启动HTTP服务器，监听中断信号，优雅关闭
// ListenAndServe表示启动HTTP服务器，监听指定的地址和端口，并处理传入的HTTP请求。
// 如果服务器启动失败，且错误不是http.ErrServerClosed，则记录错误并退出程序。
// 监听中断信号
// 优雅关闭HTTP服务器，关闭监听不在接收新请求，等待当前请求处理完成后再关闭服务器。若在10秒内未完成，则强制关闭服务器。
// ctx传给 Shutdown，作为"最多等多久"的计时器；cancel函数在Shutdown完成后调用，释放资源。
```

### 新版对应

| 原文 | 新版位置 / 说明 |
| --- | --- |
| `API key鉴权`、`创建鉴权服务` | `auth.NewKey` / `auth.NewService(keys)`；多 Key，只保存 SHA-256 哈希 |
| `配置读取`、`读取环境变量配置...` | `config.Load()`；配置项见 `internal/config/config.go` |
| `HTTP handler和代理` | `gateway.NewProxy` / `gateway.NewHandler` |
| `中间件负载均衡` | `middleware.Chain`；顺序为 request id → 访问日志 → recovery |
| `模型实例选择、负载均衡策略` | `routing.NewPicker`；平滑加权轮询 + 并发信号量 |
| `创建模型实例列表` | `instances` 循环，格式 `id|model|endpoint|weight|max_inflight` |
| `创建代理服务...` | `gateway.NewProxy(gateway.ProxyConfig{...})`，新增 MaxAttempts/MaxBodyBytes/Metrics |
| `创建HTTP handler...` | `gateway.NewHandler(gateway.HandlerConfig{...})`，新增 Limits/Health/AdminToken |
| `创建HTTP服务器...` | `http.Server`，仍设置 ReadHeaderTimeout/IdleTimeout |
| `启动HTTP服务器...`、`ListenAndServe...` | `go func() { server.ListenAndServe() }()` |
| `如果服务器启动失败...` | `err != http.ErrServerClosed` 时记录并 `os.Exit(1)` |
| `监听中断信号` | `signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)` |
| `优雅关闭HTTP服务器...` | 先 `stopChecker()`，再 `server.Shutdown(shutdownContext)` |
| `ctx传给 Shutdown...` | `shutdownContext, cancel := context.WithTimeout(...)`，`defer cancel()` |

## 2. internal/auth/service.go

### 原始注释

```go
// TrimSpace去除value前后空格，并检查是否以"Bearer "开头，如果是，则去掉前缀
// 检查value是否为空，或者经过HashKey处理后的值是否与存储的keyHash不匹配，如果是，则返回空的Principal和false，表示认证失败
```

### 新版对应

- 第一条 → `bearerToken`：去掉可选的 `Bearer ` 前缀并 `TrimSpace`。
- 第二条 → `Authenticate`：不再和单个 `keyHash` 比较，而是对全部 Key 的哈希做
  恒定时间比较；新增 `Principal.KeyID`、`Principal.Name`，返回匹配到的 `*Key`
  以便 handler 读取模型白名单、RPM 和配额。
- 认证仍然只比较 SHA-256 哈希，明文 token 不落库、不写日志。

## 3. internal/auth/service_test.go

### 原始注释

```go
// 传入服务的apiKey、租户ID和允许的模型列表，创建一个新的Service实例
// principal表示认证后的主体信息，ok表示认证是否成功
// 验证认证结果是否符合预期，确保返回的主体信息和认证状态正确
// 验证授权结果，确保服务能够正确地判断模型是否被允许
// 验证错误的API密钥是否被拒绝，确保服务能够正确地处理无效的认证请求
```

### 新版对应

- 第一条 → `NewService([]Key{...})`，现在一次传入多个 Key（含通配 Key）。
- 第二条 → `principal, key, ok := service.Authenticate(...)`。
- 第三条 → 校验 `principal.TenantID`、`principal.KeyID` 和 `key.RPM`/`DailyTokens`。
- 第四条 → 校验 `key.Allows("qwen-demo")` 为 true、`key.Allows("other")` 为 false。
- 第五条 → 校验 `Authenticate("Bearer wrong")` 返回 `ok == false`。

## 4. internal/config/config.go

### 原始注释

```go
// 限制实例同时处理的请求数量
```

### 新版对应

- 对应 `Upstream.MaxInflight`，当前从 `MGW_UPSTREAMS` 的第 5 个字段解析：
  `id|model|endpoint|weight|max_inflight`。
- 现已在字段上补回注释：`// MaxInflight 限制实例同时处理的请求数量。`

## 5. internal/gateway/handler.go

### 原始注释

```go
// 检查请求方法是否为POST，并且路径是否匹配模型路由，如果不匹配，则返回404错误
// 从请求头中获取Authorization字段，并调用Authenticate方法进行认证，返回主体信息和认证状态
// 检查租户是否有访问指定模型的权限，如果没有，则返回403错误
// 错误分类，根据不同的错误类型返回不同的HTTP状态码和错误信息，并记录日志
// 请求被客户端取消，返回499状态码
// 请求超时，返回504状态码
// 上游服务返回错误，返回502状态码
// 如果响应还没有开始写入，则返回错误信息给客户端
// 错误日志，记录请求ID、租户ID、模型、错误码、处理时间和错误信息
// 成功日志，记录请求ID、租户ID、模型、HTTP状态码、处理时间、首字节时间和响应字节数
```

### 新版对应

- 路由检查 → `if r.Method != http.MethodPost || (!ok && !canonical)`；
  新增标准路径 `/v1/chat/completions`，`model` 从 body 读取。
- 认证 → `h.auth.Authenticate(...)`；现在先认证再读 body，避免未认证请求触发大 body 缓冲。
- 模型授权 → `key.Allows(model)`，失败返回 403 `model_access_denied`。
- 错误分类 → `classifyProxyError`：
  - `context.Canceled` → 499 `client_cancelled`
  - `context.DeadlineExceeded` → 504 `upstream_timeout`
  - `ErrUpstreamUnavailable` → 503 `upstream_unavailable`
  - 其他 → 502 `upstream_error`
- “响应还没开始写入才返回错误” → `if !result.ResponseStarted { ... }`；499 已断开，不再写 body。
- 错误日志/成功日志 → 统一为 `logger.Warn/Info("request audit", ...)`，新增实例、尝试次数、
  首字节、响应字节和 prompt/completion/total token；仍不记录正文。

补充：handler 在代理前还增加了 RPM 令牌桶、每日 token 配额、inflight 和 Prometheus 指标，
这些是重构新增逻辑，原文没有对应注释。

## 6. internal/gateway/proxy.go

### 原始注释

```go
// 读取请求体，限制最大为10MB.10 << 20 = 10 * 2^20 = 10MB
// 选择一个可用的模型实例，如果没有可用实例，则返回ErrUpstreamUnavailable错误
// 设置请求上下文，如果配置了请求超时时间，则创建一个带超时的上下文
// 构建上游请求的URL，去掉实例Endpoint的尾部斜杠，并拼接请求路径和查询参数
// 创造建一个新的HTTP请求，使用上游URL、原始请求方法和请求体，并复制原始请求头，去掉Authorization字段，添加X-Forwarded-Request-ID字段
// 获取上游响应，如果请求被取消或超时，则返回相应的错误；如果上游请求失败，则标记实例为不可用，并返回错误
// 请求被标记为不健康，以便下次不再选择该实例
// 执行完后关闭响应体，以释放连接资源，避免连接泄漏
// 复制上游响应头到客户端响应头，如果是流式响应或SSE，则直接将上游响应体写入客户端响应体，并刷新缓冲区；否则读取上游响应体并写入客户端响应体
// 如果符合流式响应或SSE的条件，则直接将上游响应体写入客户端响应体，并刷新缓冲区
// 缓冲区
// 如果是第一次写入数据，则记录首字节时间
// 把上游内容写入客户端响应体，并刷新缓冲区，如果写入失败，则返回context.Canceled错误
// Flush() 的作用是将缓冲区中的数据立即发送给客户端，而不是等待缓冲区满或请求结束。这样可以减少延迟，尤其是在流式响应或SSE场景下，确保客户端能够及时接收到数据。
// 通过类型断言检查 ResponseWriter 是否实现了 http.Flusher 接口，如果实现了，则调用 Flush() 方法刷新缓冲区。
// 如果没有实现 http.Flusher 接口，则说明无法刷新缓冲区，可能会导致客户端无法及时接收到数据。
// 因此，在流式响应或SSE场景下，建议使用支持 http.Flusher 的 ResponseWriter，例如 http.ResponseWriter 或者自定义的 ResponseWriter。
// 如果读取上游响应体出错，且不是EOF，则返回读取错误；如果是EOF，则表示上游响应体已经读取完毕，返回结果
// 如果不是流式响应或SSE，就进入普通响应处理，读取上游响应体，并写入客户端响应体，如果读取或写入失败，则返回相应的错误
```

### 新版对应

- 读取 body → `ServeHTTP` 中 `io.ReadAll(io.LimitReader(r.Body, p.maxBodyBytes+1))`；
  默认上限仍是 10 MiB，但改为可配置 `MGW_MAX_BODY_BYTES`。
- 选择实例 → `p.picker.AcquireExcept(model, tried)`；没有可用实例时最终返回
  `ErrUpstreamUnavailable`。
- 设置 context 超时 → `ctx, cancel := context.WithTimeout(r.Context(), p.requestTimeout)`。
- 构建 URL → `strings.TrimRight(instance.Endpoint, "/") + upstreamPath`，再拼 `RawQuery`。
- 创建请求/复制头/去掉 Authorization → `http.NewRequestWithContext` +
  `r.Header.Clone()` + `Del("Authorization")`，并设置 `X-Forwarded-Request-ID`。
- 获取响应/取消/超时 → `p.client.Do`；先判断 `ctx.Err()`，再标记实例不健康。
- 标记不健康 → `p.picker.SetHealth(instance.ID, false)`，并增加失败指标。
- 关闭响应体 → `defer response.Body.Close()`。
- 复制响应头/流式分支 → `forward` 中的 `copyHeaders` 和
  `if stream || strings.HasPrefix(Content-Type, "text/event-stream")`。
- 缓冲区 → `make([]byte, 32*1024)`。
- 首字节 → `if firstByte { result.FirstByte = time.Since(started) }`。
- 写入/Flush → `w.Write(buffer[:read])` + `w.(http.Flusher).Flush()`；
  写失败返回 `context.Canceled`。
- 读取错误/EOF → `readErr` 分支；EOF 正常返回。
- 非流式 → `io.ReadAll(response.Body)`，再用 `usage.Parse` 解析 JSON usage。

补充：重构新增了重试循环（502/503/504 或连接错误时换实例）、显式路由改写
`/v1/models/{model}/chat/completions` → `/v1/chat/completions`、model 注入，
以及 SSE usage 追踪。

## 7. internal/middleware/middleware.go

### 原始注释

```go
// 获取请求ID，如果请求头中没有X-Request-ID，则生成一个新的请求ID
// 输出请求日志，包括请求ID、方法、路径和处理时间
// 捕获panic，输出错误日志，并返回500错误
```

### 新版对应

- 这三条注释当前仍在代码中，分别对应 `requestIDs`、`logging`、`recovery`。
- 重构修正了 `Chain` 的顺序：现在是 `recovery(requestIDs(logging(next)))`，
  保证 `logging` 能读到 request id；并新增了一条 `Chain` 顺序说明。

## 8. internal/routing/picker.go

### 原始注释

```go
// 实例最大并发数，超过该值的请求会被拒绝
// 用于保护实例状态的互斥锁，保证并发安全
// 校验实例的Endpoint是否合法，权重和最大并发数是否大于0，如果不合法则跳过该实例
// inflight是一个带缓冲的通道，用于限制实例的最大并发数，缓冲区大小为MaxInflight。chan struct{}，容量为MaxInflight
// 可以把它理解为MaxInflight个令牌桶，只有当有空闲的令牌时，才能处理新的请求。每次处理请求时，都会向inflight通道发送一个空结构体{}，表示占用一个并发槽位；处理完请求后，会从inflight通道接收一个空结构体{}，表示释放一个并发槽位。
// 上锁 防止多请求同时进入更改currentWeight
// 过滤并发已满的实例，如果当前实例的inflight通道已经满了，则跳过该实例，继续选择下一个实例
// 加权选择算法，每个实例的currentWeight会累加它的权重，选择currentWeight最大的实例作为当前请求的目标实例。
// 选择后，selected实例的currentWeight会减去所有健康实例的总权重，以保证下次选择时不会一直选择同一个实例。
// 占用一个并发槽位，返回释放函数和选择结果，如果没有可用实例，则返回false
```

### 新版对应

- 最大并发字段 → `Instance.MaxInflight`；`config.Upstream` 中同名字段负责解析。
- 互斥锁 → `Picker.mu`，保护 `instances` 和 `currentWeight`。
- 校验 endpoint/权重/并发 → `NewPicker` 中的 `url.Parse` + `Weight <= 0` +
  `MaxInflight <= 0`。
- inflight channel/令牌桶 → `state.inflight`，容量 `MaxInflight`；
  `AcquireExcept` 发送占位、返回的闭包接收释放。
- 上锁 → `p.mu.Lock()` / `defer p.mu.Unlock()`。
- 过滤并发已满 → `if len(candidate.inflight) >= cap(candidate.inflight) { continue }`。
- 加权选择 → `candidate.currentWeight += candidate.instance.Weight`，选最大者。
- 减去总权重 → `selected.currentWeight -= totalWeight`。
- 占用槽位 → `selected.inflight <- struct{}{}`；返回 `func() { <-selected.inflight }`。
- 健康实例计数 → `Healthy` / `HealthyCount`；新增 `Snapshot`、`Models`、
  `AcquireExcept` 供故障转移和指标使用。

## 重构新增的模块

以下文件是 `f522899` 新增的，原文没有对应注释；它们的职责见
[架构文档](architecture.md)：

| 文件 | 职责 |
| --- | --- |
| `internal/limits/limits.go` | RPM 令牌桶、每日 token 计数与配额 |
| `internal/usage/usage.go` | JSON / SSE 的 token usage 解析 |
| `internal/metrics/metrics.go` | Prometheus 文本指标 |
| `internal/health/checker.go` | 后台健康探测、失败下线与恢复 |
| `internal/version/version.go` | 构建版本 |
