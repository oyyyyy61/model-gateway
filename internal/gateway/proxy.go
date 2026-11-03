// Package gateway contains the HTTP surface of the gateway: routing, auth,
// admission control, proxying and audit logging.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/oyyyyy61/model-gateway/internal/metrics"
	"github.com/oyyyyy61/model-gateway/internal/routing"
	"github.com/oyyyyy61/model-gateway/internal/usage"
)

// ErrUpstreamUnavailable means no healthy instance could accept the request.
// ErrUpstreamUnavailable 表示没有健康实例可用。
var ErrUpstreamUnavailable = errors.New("upstream unavailable")

// ErrRequestTooLarge means the request body exceeded the configured limit.
// ErrRequestTooLarge 表示请求体超过 MGW_MAX_BODY_BYTES。
var ErrRequestTooLarge = errors.New("request body too large")

// proxy.go 是网关的转发核心：限制请求体、解析 stream、按模型选实例，
// 在响应开始前做故障转移，并把上游 JSON/SSE 响应原样转发给客户端。

// ProxyConfig wires a Proxy.
// ProxyConfig 装配 Proxy；MaxAttempts 默认 1，MaxBodyBytes 默认 10 MiB。
type ProxyConfig struct {
	Picker         *routing.Picker
	Client         *http.Client
	RequestTimeout time.Duration
	Logger         *slog.Logger
	MaxAttempts    int
	MaxBodyBytes   int64
	Metrics        *metrics.Metrics
}

// Proxy forwards OpenAI compatible requests to model instances.
// Proxy 负责实例选择、上游请求、重试和响应转发。
type Proxy struct {
	picker         *routing.Picker
	client         *http.Client
	requestTimeout time.Duration
	logger         *slog.Logger
	maxAttempts    int
	maxBodyBytes   int64
	metrics        *metrics.Metrics
}

// Result describes what the proxy did with a request.
// Result 记录一次代理调用的结果，供 handler 记录指标和审计。
type Result struct {
	StatusCode      int
	Result          string
	Stream          bool
	ResponseStarted bool
	ResponseBytes   int64
	FirstByte       time.Duration
	InstanceID      string
	Attempts        int
	Usage           usage.Usage
}

// NewProxy applies safe defaults for optional configuration.
// NewProxy 为 Client、MaxAttempts、MaxBodyBytes 填默认值。
func NewProxy(cfg ProxyConfig) *Proxy {
	proxy := &Proxy{
		picker:         cfg.Picker,
		client:         cfg.Client,
		requestTimeout: cfg.RequestTimeout,
		logger:         cfg.Logger,
		maxAttempts:    cfg.MaxAttempts,
		maxBodyBytes:   cfg.MaxBodyBytes,
		metrics:        cfg.Metrics,
	}
	if proxy.client == nil {
		proxy.client = http.DefaultClient
	}
	if proxy.maxAttempts < 1 {
		proxy.maxAttempts = 1
	}
	if proxy.maxBodyBytes <= 0 {
		proxy.maxBodyBytes = 10 << 20
	}
	return proxy
}

// ServeHTTP buffers the body, then tries instances until one produces a
// response that should be forwarded.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request, model string) (Result, error) {
	started := time.Now()
	// 读取请求体并限制大小；body 在重试之间复用。
	body, err := io.ReadAll(io.LimitReader(r.Body, p.maxBodyBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("read request body: %w", err)
	}
	if int64(len(body)) > p.maxBodyBytes {
		return Result{}, ErrRequestTooLarge
	}
	// 根据 body 的 stream 字段决定是否按 SSE 处理。
	stream := requestIsStream(body)

	// 在客户端 context 上叠加整体请求超时；客户端断连会取消上游请求。
	ctx := r.Context()
	if p.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.requestTimeout)
		defer cancel()
	}

	tried := make(map[string]struct{}, p.maxAttempts)
	var lastErr error
	attempts := 0
	// 最多尝试 MaxAttempts 次；每次排除已失败的实例，避免重复打同一个实例。
	for attempts < p.maxAttempts {
		// 选择一个可用实例；没有健康且有空闲并发额度的实例时退出重试。
		instance, release, ok := p.picker.AcquireExcept(model, tried)
		if !ok {
			break
		}
		attempts++
		tried[instance.ID] = struct{}{}

		result, retry, err := p.attempt(ctx, w, r, model, instance, body, stream, started, attempts < p.maxAttempts)
		// 无论成功、失败、超时都释放并发额度。
		release()
		result.Attempts = attempts

		// 不需要重试（成功或响应已开始）时直接返回。
		if !retry {
			return result, err
		}
		lastErr = err
		if p.logger != nil {
			p.logger.Warn("upstream attempt failed, trying another instance", "model", model, "instance", instance.ID, "attempt", attempts, "error", err)
		}
	}

	// 没有可用实例或全部失败：返回 lastErr / ErrUpstreamUnavailable。
	if lastErr != nil {
		return Result{Attempts: attempts}, lastErr
	}
	return Result{Attempts: attempts}, ErrUpstreamUnavailable
}

func (p *Proxy) attempt(ctx context.Context, w http.ResponseWriter, r *http.Request, model string, instance routing.Instance, body []byte, stream bool, started time.Time, allowRetry bool) (Result, bool, error) {
	// 显式路由 /v1/models/{model}/chat/completions 改写为上游标准路径，
	// 并在请求体缺少 model 时注入 URL 中的模型。
	// The explicit /v1/models/{model}/chat/completions route is translated to
	// the canonical OpenAI path so vLLM and GPUStack instances can serve it.
	// The model is injected into the body when the caller used the explicit
	// route without setting it.
	requestBody := body
	upstreamPath := r.URL.Path
	if strings.HasPrefix(upstreamPath, "/v1/models/") {
		upstreamPath = "/v1/chat/completions"
		requestBody = ensureModel(body, model)
	}
	// 构建上游 URL：去掉 endpoint 尾部斜杠，再拼请求路径和查询参数。
	upstreamURL := strings.TrimRight(instance.Endpoint, "/") + upstreamPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	// 创建上游请求：复用原方法/请求体，复制请求头，去掉网关的 Authorization。
	request, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL, bytes.NewReader(requestBody))
	if err != nil {
		return Result{InstanceID: instance.ID}, false, fmt.Errorf("create upstream request: %w", err)
	}
	request.Header = r.Header.Clone()
	// 不把租户 API Key 泄露给模型服务。
	request.Header.Del("Authorization")
	request.Header.Del("Content-Length")
	request.ContentLength = int64(len(requestBody))
	request.Header.Set("X-Forwarded-Request-ID", r.Header.Get("X-Request-ID"))

	// 发起上游请求；客户端取消/超时优先返回 context 错误，不标记实例不健康。
	response, err := p.client.Do(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Result{InstanceID: instance.ID}, false, context.DeadlineExceeded
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return Result{InstanceID: instance.ID}, false, context.Canceled
		}
		// 连接失败：标记实例不健康，交给下一次探测恢复。
		p.picker.SetHealth(instance.ID, false)
		if p.metrics != nil {
			p.metrics.UpstreamFailures.Inc(map[string]string{"instance": instance.ID})
		}
		return Result{InstanceID: instance.ID}, true, fmt.Errorf("upstream request: %w", err)
	}

	// 502/503/504 且还有重试机会：丢弃响应体，换一个实例。
	if allowRetry && isRetryableStatus(response.StatusCode) {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 32<<10))
		_ = response.Body.Close()
		if p.metrics != nil {
			p.metrics.UpstreamFailures.Inc(map[string]string{"instance": instance.ID})
		}
		return Result{InstanceID: instance.ID, StatusCode: response.StatusCode, Result: classifyStatus(response.StatusCode)}, true, nil
	}

	// 非重试响应交给 forward：写状态码、转发 body/SSE，并解析 usage。
	result, err := p.forward(w, response, stream, started)
	result.InstanceID = instance.ID
	return result, false, err
}

func (p *Proxy) forward(w http.ResponseWriter, response *http.Response, stream bool, started time.Time) (Result, error) {
	// 执行完关闭响应体，释放连接资源。
	defer response.Body.Close()
	// 复制上游响应头（过滤逐跳头）到客户端。
	copyHeaders(w.Header(), response.Header)

	// 流式/SSE：边读上游、边写客户端并 Flush；非流式在下面整体读取后写出。
	if stream || strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		result := Result{
			StatusCode:      response.StatusCode,
			Result:          classifyStatus(response.StatusCode),
			Stream:          true,
			ResponseStarted: true,
		}
		// 先写状态码；SSE 开始后只能继续写 body，不能再补 JSON 错误。
		w.WriteHeader(response.StatusCode)
		// StreamTracker 从 data: 行里提取最后一个 usage。
		tracker := &usage.StreamTracker{}
		buffer := make([]byte, 32*1024)
		firstByte := true
		for {
			read, readErr := response.Body.Read(buffer)
			if read > 0 {
				// 第一次写入时记录首字节时间。
				if firstByte {
					result.FirstByte = time.Since(started)
					firstByte = false
				}
				// 把上游内容写入客户端；写失败说明客户端已断开。
				written, writeErr := w.Write(buffer[:read])
				result.ResponseBytes += int64(written)
				_, _ = tracker.Write(buffer[:read])
				// Flush 立即把缓冲区数据发给客户端，降低 SSE 延迟。
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				if writeErr != nil {
					result.Usage, _ = tracker.Usage()
					return result, context.Canceled
				}
			}
			// 读取结束：EOF 表示正常完成；其他错误原样返回。
			if readErr != nil {
				result.Usage, _ = tracker.Usage()
				if errors.Is(readErr, io.EOF) {
					return result, nil
				}
				return result, readErr
			}
		}
	}

	// 非流式响应：整体读取后一次性写出，并从 JSON 中解析 usage。
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return Result{StatusCode: response.StatusCode, Result: classifyStatus(response.StatusCode)}, err
	}
	result := Result{
		StatusCode:      response.StatusCode,
		Result:          classifyStatus(response.StatusCode),
		ResponseStarted: true,
		ResponseBytes:   int64(len(data)),
		FirstByte:       time.Since(started),
	}
	if parsed, ok := usage.Parse(data); ok {
		result.Usage = parsed
	}
	w.WriteHeader(response.StatusCode)
	written, writeErr := w.Write(data)
	result.ResponseBytes = int64(written)
	return result, writeErr
}

// requestIsStream 只解析 body 的 stream 字段，决定是否走 SSE 转发。
func requestIsStream(body []byte) bool {
	var payload struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &payload) == nil && payload.Stream
}

// ensureModel adds the model field when the caller selected it in the URL and
// left it out of the JSON body.
// ensureModel 在显式路由且 body 没有 model 时注入 URL 中的模型。
func ensureModel(body []byte, model string) []byte {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	if existing, ok := payload["model"]; ok && len(existing) > 0 && string(existing) != "null" && string(existing) != `""` {
		return body
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return body
	}
	payload["model"] = encoded
	updated, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return updated
}

// MaxBodyBytes exposes the configured request body limit to the handler.
// MaxBodyBytes 暴露请求体上限，供 handler 提前做限制。
func (p *Proxy) MaxBodyBytes() int64 {
	return p.maxBodyBytes
}

// hopByHopHeaders 是 RFC 7230 的逐跳头，代理不能转发。
var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// copyHeaders 复制响应头，跳过逐跳头。
func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if _, skip := hopByHopHeaders[http.CanonicalHeaderKey(key)]; skip {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// isRetryableStatus 只对 502/503/504 做故障转移。
func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// classifyStatus buckets an upstream status for logs and metrics.
// classifyStatus 把上游状态码归类为 success/upstream_4xx/upstream_5xx。
func classifyStatus(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "success"
	case status >= 400 && status < 500:
		return "upstream_4xx"
	default:
		return "upstream_5xx"
	}
}
