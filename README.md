# Model Gateway

面向内部业务方的多租户模型服务网关首阶段实现。当前版本提供：

- API Key 鉴权与租户模型授权
- OpenAI 风格 JSON 请求代理
- SSE 流式响应透传
- 健康实例选择与本地并发上限
- 请求超时、客户端取消传播和结构化日志
- `/healthz`、`/readyz`、`/metrics`
- Docker Compose 双模拟模型实例
- MySQL 首版迁移文件，以及 Redis/MySQL/Prometheus 的基础设施 profile

## 快速启动

本机已安装 Go 1.24+ 时可以直接运行：

```bash
go test ./...
go run ./cmd/gateway
```

也可以使用 Docker：

```bash
docker compose up --build gateway mock-model-1 mock-model-2
```

调用普通 JSON 接口：

```bash
curl http://localhost:8080/v1/models/qwen-demo/chat/completions \
  -H 'Authorization: Bearer mgw_live_demo' \
  -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"介绍一下 Go 的 context"}]}'
```

调用 SSE 接口：

```bash
curl -N http://localhost:8080/v1/models/qwen-demo/chat/completions \
  -H 'Authorization: Bearer mgw_live_demo' \
  -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"hello"}],"stream":true}'
```

启用 Redis、MySQL 和 Prometheus 容器：

```bash
docker compose --profile infra up --build
```

## 配置

主要环境变量：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `MGW_LISTEN_ADDR` | `:8080` | 网关监听地址 |
| `MGW_API_KEY` | `mgw_live_demo` | 首阶段演示 API Key |
| `MGW_TENANT_ID` | `tenant-demo` | 首阶段演示租户 |
| `MGW_ALLOWED_MODELS` | `qwen-demo` | 逗号分隔的授权模型 |
| `MGW_REQUEST_TIMEOUT` | `120s` | 整体请求超时 |
| `MGW_UPSTREAMS` | 见 compose | `id|model|endpoint|weight|max_inflight`，多个实例用分号分隔 |

当前认证和实例配置使用内存实现，便于完成阶段一和单元测试。Redis/MySQL 容器和迁移文件已经作为基础设施入口保留，业务连接、跨实例限流、审计写入、实例后台健康探测和管理接口安排在后续阶段。

## 测试

```bash
go test ./...
```

在没有 Go 环境的机器上使用 Docker 测试：

```bash
docker run --rm -v "${PWD}:/src" -w /src golang:1.24-alpine go test ./...
```

## 绑定 GitHub

1. 在 GitHub 新建一个空仓库，例如 `model-gateway`。创建时不要添加 README、`.gitignore` 或 license，便于与本地目录合并。
2. 在本地项目目录执行：

```bash
git init
git add .
git commit -m "feat: bootstrap model gateway"
git branch -M main
git remote add origin https://github.com/<你的用户名>/model-gateway.git
git push -u origin main
```

3. 如果使用 SSH：

```bash
git remote set-url origin git@github.com:<你的用户名>/model-gateway.git
git push -u origin main
```

4. 验证绑定：

```bash
git remote -v
git status
```

首次使用 SSH 时，需要先把本机公钥添加到 GitHub 的 **Settings → SSH and GPG keys**。HTTPS 推送需要 GitHub Personal Access Token，账号密码方式已被 GitHub 移除。

当前 Go module 路径已经与远程仓库保持一致：`github.com/oyyyyy61/model-gateway`。
