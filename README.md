# Model Gateway

面向 GPU 集群的多租户 LLM 网关：对业务方提供 OpenAI 兼容 API，对内统一管理
vLLM / GPUStack 实例的鉴权、限流、配额、路由、故障转移、用量审计和可观测性。

当前版本（`0.2.0`）在原有"网关 + 错误透传"骨架上补齐了生产化所需的核心能力，并与
`alertment`、`Prometheus`、`ai-job-Operator` 的观测与 GPU 生态对齐。

## 功能

- **多租户 API Key**：一个进程配置多个 Key，每个 Key 独立的租户、模型白名单、
  RPM 和每日 token 配额；支持 `*` 通配模型。
- **OpenAI 兼容路由**：`POST /v1/chat/completions` 按请求体 `model` 路由；
  兼容显式路径 `/v1/models/{model}/chat/completions`，并自动重写为上游标准路径。
- **限流与配额**：RPM 令牌桶（允许突发）、每日 token 软配额（UTC 跨天重置）、
  `429` + `Retry-After`。
- **负载均衡与故障转移**：平滑加权轮询、实例并发上限、连接错误/502/503/504
  自动换实例；SSE 开始后不再重试，避免污染流。
- **健康探测**：后台周期探测 `endpoint + MGW_HEALTH_PATH`，连续失败下线、连续
  成功恢复；`/readyz` 直接反映上游可用性。
- **用量与审计**：解析非流式 JSON 与流式 SSE 的 `usage`，结构化 audit 日志记录
  租户、Key、模型、实例、耗时、首字节、token 用量，不记录正文。
- **可观测性**：Prometheus 文本指标、告警规则、Grafana 面板、飞书告警接入示例。
- **部署资产**：Docker Compose、Kubernetes（Deployment / Service / HPA /
  PodMonitor / PrometheusRule / Grafana Dashboard）。

## 架构

```mermaid
flowchart LR
    C[业务方 / OpenAI SDK / 评测任务] -->|Bearer Key| G[model-gateway]
    G --> A[认证 + 模型授权]
    A --> L[RPM / 每日 token 配额]
    L --> P[加权选实例 + 并发上限]
    P --> V1[vLLM 实例 A]
    P --> V2[vLLM 实例 B]
    H[健康探测器] --> V1
    H --> V2
    G --> M[/metrics]
    M --> PR[Prometheus]
    PR --> AM[Alertmanager]
    AM --> FS[feishu-adapter]
    PR --> GF[Grafana]
    PR --> AIJ[ai-job-Operator / GPU 指标]
```

详细设计见 [架构文档](docs/architecture.md)，分阶段任务见 [路线图](docs/roadmap.md)。

## 快速开始

### Docker Compose（推荐）

```bash
docker compose up --build gateway mock-model-1 mock-model-2
```

网关监听 `http://localhost:8080`，两个 mock 模型实例模拟 vLLM 的
`/v1/chat/completions` 与 SSE 响应。

```bash
# 标准 OpenAI 调用
curl http://localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer mgw_live_demo' \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen-demo","messages":[{"role":"user","content":"你好"}]}'

# SSE 流式调用
curl -N http://localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer mgw_live_demo' \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen-demo","messages":[{"role":"user","content":"hello"}],"stream":true}'

# 显式模型路径（网关会重写为 /v1/chat/completions 并注入 model）
curl http://localhost:8080/v1/models/qwen-demo/chat/completions \
  -H 'Authorization: Bearer mgw_live_demo' \
  -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"hello"}]}'
```

### 本机 Go 运行

```bash
# 终端 1：mock 模型
PORT=8081 go run ./cmd/mock-model

# 终端 2：网关
MGW_UPSTREAMS='mock|qwen-demo|http://127.0.0.1:8081|100|8' \
MGW_API_KEYS='mgw_live_demo|tenant-demo|qwen-demo|120|1000000|demo' \
MGW_ADMIN_TOKEN='admin-demo' \
go run ./cmd/gateway
```

Windows PowerShell 可使用 `$env:PORT='8081'; go run ./cmd/mock-model` 等等价写法。

### 验证

```bash
curl -s localhost:8080/healthz
curl -s localhost:8080/readyz
curl -s -H 'Authorization: Bearer mgw_live_demo' localhost:8080/v1/models
curl -s localhost:8080/metrics | grep '^gateway_'
curl -s -H 'Authorization: Bearer admin-demo' localhost:8080/admin/status
```

## API

| 方法 | 路径 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| POST | `/v1/chat/completions` | Bearer Key | 标准 OpenAI 接口，按 body 的 `model` 路由 |
| POST | `/v1/models/{model}/chat/completions` | Bearer Key | 显式模型路由，兼容旧调用方式 |
| GET | `/v1/models` | Bearer Key | 列出该 Key 可访问的模型 |
| GET | `/healthz` | 无 | 进程存活 |
| GET | `/readyz` | 无 | 至少一个上游健康时返回 200，否则 503 |
| GET | `/metrics` | 无 | Prometheus 指标 |
| GET | `/admin/status` | Admin Token | 版本、运行时长、上游健康、Key 数量 |

错误码：

| HTTP | code | 含义 |
| --- | --- | --- |
| 400 | `missing_model` / `invalid_request` | 缺少 model 或请求体不是 JSON |
| 401 | `unauthorized` | Key 或 Admin Token 无效 |
| 403 | `model_access_denied` | Key 无权访问该模型 |
| 413 | `request_too_large` | 请求体超过 `MGW_MAX_BODY_BYTES` |
| 429 | `rate_limited` | 超过 RPM，带 `Retry-After` |
| 429 | `quota_exceeded` | 超过每日 token 配额 |
| 499 | `client_cancelled` | 客户端取消（日志语义，不返回 body） |
| 502 | `upstream_error` | 上游请求失败 |
| 503 | `upstream_unavailable` | 无健康实例 |
| 504 | `upstream_timeout` | 上游超时 |

## 配置

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `MGW_LISTEN_ADDR` | `:8080` | 监听地址 |
| `MGW_API_KEYS` | 空 | `token\|tenant\|models\|rpm\|daily_tokens\|name`，分号分隔多个；`models` 用逗号分隔，`*` 表示全部 |
| `MGW_API_KEY` | `mgw_live_demo` | 旧版单 Key；`MGW_API_KEYS` 为空时生效 |
| `MGW_TENANT_ID` | `tenant-demo` | 旧版租户 |
| `MGW_ALLOWED_MODELS` | `qwen-demo` | 旧版模型白名单 |
| `MGW_RPM` | `0` | 旧版 RPM，0 表示不限 |
| `MGW_DAILY_TOKENS` | `0` | 旧版每日 token 配额，0 表示不限 |
| `MGW_UPSTREAMS` | 两个 mock 实例 | `id\|model\|endpoint\|weight\|max_inflight`，分号分隔 |
| `MGW_REQUEST_TIMEOUT` | `120s` | 单次请求总超时（含重试） |
| `MGW_HEALTH_INTERVAL` | `15s` | 健康探测间隔，`0` 关闭 |
| `MGW_HEALTH_PATH` | `/healthz` | 探测路径；vLLM 用 `/health` |
| `MGW_HEALTH_FAILURES` | `2` | 连续失败多少次下线 |
| `MGW_HEALTH_SUCCESSES` | `2` | 连续成功多少次恢复 |
| `MGW_RETRY_ATTEMPTS` | `2` | 单请求最大尝试次数 |
| `MGW_ADMIN_TOKEN` | 空 | `/admin/status` Token；为空时该路由 404 |
| `MGW_MAX_BODY_BYTES` | `10485760` | 请求体上限 |
| `MGW_SHUTDOWN_TIMEOUT` | `10s` | 优雅关闭等待时间 |
| `MGW_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

`MGW_UPSTREAMS` 的 `endpoint` 必须是 OpenAI 兼容 API 的根地址（如
`http://vllm:8000`）。网关统一向上游发送 `/v1/chat/completions`，因此 vLLM、
GPUStack、mock 都可以直接接入。

## 可观测性

`/metrics` 暴露：

| 指标 | 类型 | 标签 |
| --- | --- | --- |
| `gateway_requests_total` | counter | model, tenant, status |
| `gateway_request_duration_seconds` | histogram | model |
| `gateway_tokens_total` | counter | tenant, model, kind |
| `gateway_inflight_requests` | gauge | model |
| `gateway_upstream_healthy` | gauge | instance, model |
| `gateway_rate_limited_total` | counter | tenant, reason |
| `gateway_upstream_failures_total` | counter | instance |
| `gateway_health_checks_total` | counter | instance, result |
| `gateway_build_info` | gauge | version |

仓库内已提供：

- `deploy/prometheus/rules.yml`：本地 Prometheus 告警规则。
- `deploy/k8s/prometheusrule.yaml`：Kubernetes PrometheusRule。
- `deploy/k8s/podmonitor.yaml`：kube-prometheus-stack 抓取配置。
- `deploy/k8s/grafana-dashboard.json`：请求、错误率、P95、token、上游健康面板。
- `deploy/k8s/alertmanagerconfig.yaml`：把 `category=model-gateway` 的告警送到
  `alertment` 仓库的飞书适配器。

告警统一带 `category: model-gateway`，可直接接入 `alertment` 既有的
Alertmanager 路由与飞书通知链路。

## Kubernetes 部署

```bash
# 1. 创建命名空间（kustomize 也会创建）
kubectl create namespace model-gateway

# 2. 创建真实密钥（不要使用 secret.example.yaml 的占位值）
kubectl -n model-gateway create secret generic model-gateway-secrets \
  --from-literal=MGW_API_KEYS='<token>|<tenant>|<models>|<rpm>|<daily_tokens>|<name>' \
  --from-literal=MGW_ADMIN_TOKEN='<admin-token>'

# 3. 按实际镜像与上游地址修改 configmap.yaml 后部署
kubectl apply -k deploy/k8s

# 4. 验证
kubectl -n model-gateway get pod,svc,hpa
kubectl -n model-gateway port-forward svc/model-gateway 8080:80
```

部署清单包含 2 副本、滚动发布、HPA、readiness/liveness、非 root、只读根文件系统，
以及 PodMonitor / PrometheusRule / Grafana Dashboard。

## 与其它仓库的整合

| 仓库 | 用途 |
| --- | --- |
| [`alertment`](https://github.com/oyyyyy61/alertment) | 复用 GPU 集群告警、飞书适配器与 Grafana 约定 |
| [`Prometheus`](https://github.com/oyyyyy61/Prometheus) | vLLM/NPU/KV cache 指标与恢复控制器，和网关 token/延迟指标关联 |
| [`ai-job-Operator`](https://github.com/oyyyyy61/ai-job-Operator) | 离线 GPU 任务与在线推理共享集群；统一归属标签 |
| [`skilling`](https://github.com/oyyyyy61/skilling) | Agent/Skill 评测流量作为独立租户接入，配额隔离 |

## 项目结构

```text
cmd/gateway/            网关入口与依赖装配
cmd/mock-model/         本地 mock 模型（JSON + SSE + usage）
internal/auth/          Key 哈希、认证、模型授权
internal/limits/        RPM 令牌桶、每日 token 配额
internal/routing/       加权轮询、并发信号量、健康状态
internal/health/        后台健康探测与恢复
internal/usage/         JSON/SSE usage 解析
internal/metrics/       Prometheus 指标
internal/gateway/       handler、proxy、故障转移、审计
internal/middleware/    request id、recovery、访问日志
internal/config/        环境变量配置
internal/version/       版本号
deploy/k8s/             Kubernetes 清单
deploy/prometheus/      本地 Prometheus 配置与规则
docs/                   架构与路线图
migrations/             MySQL 迁移（P1 持久化）
```

## 测试

```bash
go test ./...
go vet ./...
gofmt -l .
```

没有 Go 环境时：

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.24-alpine go test ./...
```

## 已知边界

- Key、限流、配额均为进程内状态，多副本不共享；P1 引入 Redis/MySQL。
- 审计只写 stdout，未落 `request_audits` 表。
- 仅实现 chat completions；embeddings、rerank、audio 未覆盖。
- 每日 token 配额是软配额，并发场景下可能短暂超出。
- 健康探测路径需按运行时配置（vLLM 为 `/health`）。

这些边界和后续任务见 [路线图](docs/roadmap.md)。
