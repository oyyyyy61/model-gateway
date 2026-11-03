# 从雏形到可上线：仓库整合路线图

本文回答两个问题：

1. `oyyyyy61` 名下几个仓库各自有什么、能拼成什么；
2. `model-gateway` 从当前雏形到"较完整项目"还缺哪些任务，按什么顺序做、怎么验收。

## 1. 现状判断

当前 `model-gateway` 已经具备"网关 + 错误传输"的基础能力：API Key、单模型授权、
加权选实例、并发上限、SSE 透传、超时/取消、健康检查、结构化日志。但它还缺四类
东西：

| 缺口 | 表现 | 影响 |
| --- | --- | --- |
| 租户治理 | 只有单 Key、无配额、无用量 | 不能给多个业务方使用 |
| 生产可靠性 | 无后台探测恢复、无故障转移、无分布式状态 | 单实例故障会放大 |
| 可观测性 | `/metrics` 只有 `gateway_up` | 无法告警、无法定位 |
| 生态整合 | 与 GPUStack、AIJob、飞书告警、Grafana 无连接 | 只是一个孤立代理 |

## 2. 仓库整合矩阵

| 仓库 | 核心资产 | 拼到网关的哪一层 | 产出 |
| --- | --- | --- | --- |
| `model-gateway` | Go HTTP 代理、路由、鉴权骨架 | 数据面 | 统一 LLM 入口 |
| [`alertment`](https://github.com/oyyyyy61/alertment) | PrometheusRule、Alertmanager + 飞书适配器、Grafana、GPU 归属标签 | 观测面 | 网关指标接入现有 GPU 集群告警 |
| [`Prometheus`](https://github.com/oyyyyy61/Prometheus) | vLLM/NPU 指标桥、KV cache、token 指标、恢复控制器、预算化观测 | 观测面 + 可靠性 | 网关流量与模型侧吞吐关联、SLO |
| [`ai-job-Operator`](https://github.com/oyyyyy61/ai-job-Operator) | AIJob CRD、Volcano、GPU 注入、优先级 | 资源面 | 在线推理与离线任务的 GPU 共享 |
| [`skilling`](https://github.com/oyyyyy61/skilling) | Agent/Skill 评测、长上下文实验 | 流量面 | 独立租户的评测流量与配额隔离 |

整合后的目标系统：

```text
业务 / 评测 / Agent
        │  OpenAI API + Bearer Key
        ▼
  model-gateway（鉴权、限流、配额、路由、审计、指标）
        │
        ├── vLLM / GPUStack 在线推理实例
        │        └── Prometheus ← dcgm / vLLM / NPU / DCGM exporter
        │
        ├── ai-job-Operator（离线训练 / 批量推理 / 模型加载）
        │        └── Volcano + GPU 节点
        │
        └── Prometheus → Alertmanager → feishu-adapter → 飞书
```

## 3. 阶段与任务

任务编号用于 issue / PR 标题，例如 `[G-03] 后台健康探测`。状态以本次提交为准。

### P0 网关核心（本次已完成）

| 编号 | 任务 | 来源 | 验收标准 | 状态 |
| --- | --- | --- | --- | --- |
| G-01 | 多租户 API Key 与模型授权 | 原骨架 | 多 Key 并存、`*` 通配、错误 Key 401、越权 403 | ✅ |
| G-02 | 标准 OpenAI 路由 | 生态接入 | `POST /v1/chat/completions` 按 body 的 model 路由；显式 URL 路由兼容 | ✅ |
| G-03 | 上游路径重写与 model 注入 | vLLM 兼容 | 显式路由转发到 `/v1/chat/completions`，缺 model 时自动注入 | ✅ |
| G-04 | RPM 令牌桶 | 租户治理 | 突发可配置、超限 429、`Retry-After` 正确 | ✅ |
| G-05 | 每日 token 配额 | 租户治理 | 响应后累计、UTC 跨天重置、超限 429 | ✅ |
| G-06 | JSON/SSE 用量解析 | 计费基础 | 非流式与流式都能提取 prompt/completion/total | ✅ |
| G-07 | 后台健康探测与恢复 | 生产可靠性 | 连续失败下线、连续成功恢复、`/readyz` 反映状态 | ✅ |
| G-08 | 故障转移 | 生产可靠性 | 连接错误/502/503/504 换实例；SSE 开始后不重试 | ✅ |
| G-09 | Prometheus 指标 | 可观测性 | 请求、耗时、token、健康、限流、探测指标齐全 | ✅ |
| G-10 | 审计日志 | 合规基础 | 每次请求一条结构化 audit，含 request id、用量、实例 | ✅ |
| G-11 | 管理端点 | 运维 | `/admin/status` 恒定时间鉴权，未配置时 404 | ✅ |
| G-12 | K8s 与告警清单 | 部署 | `kubectl apply -k deploy/k8s` 可部署；告警规则可加载 | ✅ |

P0 的已知边界（有意保留）：

- 限流/配额/Key 均为进程内状态，多副本不共享。
- 审计只写 stdout，不落 MySQL。
- 没有模型别名、按 header 路由、内容安全、OIDC。
- 只支持 chat completions，不含 embeddings / rerank / audio。

### P1 生产化基础（下一阶段，建议 2–3 周）

| 编号 | 任务 | 来源 | 验收标准 |
| --- | --- | --- | --- |
| G-13 | Redis 分布式限流与配额 | 新增 | 多副本共享 RPM/日配额；Redis 不可用时降级为本地并告警 |
| G-14 | MySQL Key/租户存储 + 管理 API | `migrations/001_init.up.sql` | Key 可创建/吊销/轮换；管理 API 需 admin token；缓存 TTL |
| G-15 | 审计落库 | `request_audits` 表 | 批量异步写入；失败重试；不阻塞主请求 |
| G-16 | 配置热更新 | K8s ConfigMap | 实例/权重/Key 变更无需重启；无效配置拒绝并保留旧值 |
| G-17 | 请求级超时与预算 | 现有 context | 支持 `X-Request-Timeout`；总预算覆盖重试，避免重试放大延迟 |
| G-18 | 模型别名与灰度 | 业务需求 | `qwen` → `qwen-72b-v2`；按租户/百分比灰度；可回滚 |
| G-19 | 内容安全与脱敏钩子 | 合规 | 可插拔 pre/post hook；超时与失败策略可配置；默认关闭 |

### P2 与 GPUStack / AIJob 打通（2–4 周）

| 编号 | 任务 | 来源 | 验收标准 |
| --- | --- | --- | --- |
| I-01 | 模型实例注册同步 | GPUStack API | 从 GPUStack 拉取实例与模型，自动生成 upstream；变更秒级生效 |
| I-02 | 模型加载/卸载任务化 | `ai-job-Operator` | 用 AIJob 表达"加载某模型到某节点"；状态回写；失败可重试 |
| I-03 | 在线/离线 GPU 配额 | `ai-job-Operator` + Volcano | 推理保底 GPU、离线任务可借用；优先级与抢占策略明确 |
| I-04 | 批量评测任务模板 | `skilling` | 评测 Job 通过网关调用模型；按租户配额隔离；结果归档 |
| I-05 | 统一归属标签 | `alertment` | 网关、AIJob、GPU 进程共用 `owner/team/project/task` 标签，告警可定位到人 |

### P3 可观测性与 SRE 闭环（持续）

| 编号 | 任务 | 来源 | 验收标准 |
| --- | --- | --- | --- |
| O-01 | SLO 定义 | `alertment` | 可用性 99.9%、P95 首字节 < 2s、错误预算可视化 |
| O-02 | 多窗口燃烧率告警 | `Prometheus/research` | 1h/6h 多窗口；误报率低于阈值；有 runbook 链接 |
| O-03 | Grafana 仪表盘 | `Prometheus/grafana` | 请求、延迟、token、上游健康、限流、GPU 关联一屏 |
| O-04 | 故障演练 | 新增 | 上游全挂、Redis 挂、限流打满、SSE 断连四类演练通过 |
| O-05 | 成本与配额报表 | `skilling` + 计费 | 按租户/模型出 token 与费用日报；异常用量告警 |
| O-06 | 容量规划 | 观测数据 | 根据 token/s 与 GPU 利用率给出扩容建议 |

## 4. 里程碑

| 里程碑 | 包含 | 完成标准 |
| --- | --- | --- |
| M1 可对内试用 | P0 | 两个业务租户接入；限流/配额生效；告警可达飞书 |
| M2 可生产多副本 | P1 | 滚动发布不丢请求；多副本共享限流；审计可查 30 天 |
| M3 与 GPU 平台一体 | P2 | 模型实例自动注册；在线/离线共享 GPU；归属标签贯通 |
| M4 可运营 | P3 | SLO、燃烧率告警、成本报表、演练常态化 |

## 5. 推荐实施顺序

1. **先接真实 vLLM**：把 `MGW_UPSTREAMS` 指向一个 vLLM 实例，验证
   `/v1/chat/completions`、SSE、usage、`/health`。
2. **再上多副本**：G-13 + G-14，否则多副本下限流会失真。
3. **然后接告警**：把 `deploy/k8s/prometheusrule.yaml` 合入 `alertment` 的
   PrometheusRule，复用飞书适配器。
4. **最后做 AIJob 整合**：I-02/I-03 涉及调度策略，放在有真实 GPU 压测数据之后。

## 6. 风险与对策

| 风险 | 影响 | 对策 |
| --- | --- | --- |
| 多副本本地限流失真 | 租户超用 | P1 优先做 Redis；过渡期限流值按副本数折算 |
| 上游 usage 缺失 | 配额/计费不准 | 记录 `usage_missing` 指标；对关键模型强制 `include_usage` |
| 健康探测误判 | 实例被误下线 | 失败/成功阈值可配；探测超时短于请求超时 |
| 重试放大 | 故障时流量翻倍 | 限制重试次数；只对幂等失败重试；记录 attempts |
| 标签基数爆炸 | Prometheus 内存 | 指标只用 model/tenant/instance；租户数超阈值时改聚合 |
| GPU 共享冲突 | 推理被离线任务挤占 | Volcano 队列 + 优先级；推理保底配额 |

## 7. 本次提交对应的验收命令

```bash
gofmt -l .
go vet ./...
go test ./...
go build ./...
```

端到端（本地）：

```bash
go run ./cmd/mock-model &          # 需要先设置 PORT
go run ./cmd/gateway &
curl -s localhost:8080/readyz
curl -s -H 'Authorization: Bearer mgw_live_demo' \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen-demo","messages":[{"role":"user","content":"hi"}]}' \
  localhost:8080/v1/chat/completions
curl -s localhost:8080/metrics | grep gateway_
```
