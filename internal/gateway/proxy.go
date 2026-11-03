// proxy.go 是网关的核心代理模块。它先读取并限制请求体大小，根据 stream 字段判断普通响应还是 SSE 响应，然后通过 Picker 选择健康且有并发容量的模型实例。
// 上游请求使用继承自客户端的 Context，并叠加整体超时，因此客户端断连和请求超时都能取消上游 HTTP 请求。
// 代理会移除网关的 Authorization Header，避免 API Key 泄露给模型服务。
// 普通响应会整体转发，SSE 响应通过循环读取、写入和 Flush 实现实时传输。
// 实例并发额度通过 defer release() 保证释放，响应已经开始后不会再写入 JSON 错误。

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

	"github.com/oyyyyy61/model-gateway/internal/routing"
)

var ErrUpstreamUnavailable = errors.New("upstream unavailable")

type ProxyConfig struct {
	Picker         *routing.Picker
	Client         *http.Client
	RequestTimeout time.Duration
	Logger         *slog.Logger
}

type Proxy struct {
	picker         *routing.Picker
	client         *http.Client
	requestTimeout time.Duration
	logger         *slog.Logger
}

type Result struct {
	StatusCode      int
	Result          string
	Stream          bool
	ResponseStarted bool
	ResponseBytes   int64
	FirstByte       time.Duration
}

// 把proxyconfig配置转换成 Proxy 对象
func NewProxy(cfg ProxyConfig) *Proxy {
	return &Proxy{picker: cfg.Picker, client: cfg.Client, requestTimeout: cfg.RequestTimeout, logger: cfg.Logger}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request, model string) (Result, error) {
	started := time.Now()
	// 读取请求体，限制最大为10MB.10 << 20 = 10 * 2^20 = 10MB
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		return Result{}, fmt.Errorf("read request body: %w", err)
	}
	stream := requestIsStream(body)
	// 选择一个可用的模型实例，如果没有可用实例，则返回ErrUpstreamUnavailable错误
	instance, release, ok := p.picker.Acquire(model)
	if !ok {
		return Result{}, ErrUpstreamUnavailable
	}
	defer release()

	// 设置请求上下文，如果配置了请求超时时间，则创建一个带超时的上下文
	ctx := r.Context()
	if p.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.requestTimeout)
		defer cancel()
	}
	// 构建上游请求的URL，去掉实例Endpoint的尾部斜杠，并拼接请求路径和查询参数
	upstreamURL := strings.TrimRight(instance.Endpoint, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	// 创造建一个新的HTTP请求，使用上游URL、原始请求方法和请求体，并复制原始请求头，去掉Authorization字段，添加X-Forwarded-Request-ID字段
	request, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("create upstream request: %w", err)
	}
	request.Header = r.Header.Clone()
	request.Header.Del("Authorization")
	request.Header.Set("X-Forwarded-Request-ID", r.Header.Get("X-Request-ID"))

	// 获取上游响应，如果请求被取消或超时，则返回相应的错误；如果上游请求失败，则标记实例为不可用，并返回错误
	response, err := p.client.Do(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Result{}, context.DeadlineExceeded
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return Result{}, context.Canceled
		}
		// 请求被标记为不健康，以便下次不再选择该实例
		p.picker.SetHealth(instance.ID, false)
		return Result{}, fmt.Errorf("upstream request: %w", err)
	}
	// 执行完后关闭响应体，以释放连接资源，避免连接泄漏
	defer response.Body.Close()

	// 复制上游响应头到客户端响应头，如果是流式响应或SSE，则直接将上游响应体写入客户端响应体，并刷新缓冲区；否则读取上游响应体并写入客户端响应体
	copyHeaders(w.Header(), response.Header)
	// 如果符合流式响应或SSE的条件，则直接将上游响应体写入客户端响应体，并刷新缓冲区
	if stream || strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		result := Result{StatusCode: response.StatusCode, Result: classifyStatus(response.StatusCode), Stream: true, ResponseStarted: true}
		w.WriteHeader(response.StatusCode)
		firstByte := true
		// 缓冲区
		buffer := make([]byte, 32*1024)
		for {
			read, readErr := response.Body.Read(buffer)
			if read > 0 {
				// 如果是第一次写入数据，则记录首字节时间
				if firstByte {
					result.FirstByte = time.Since(started)
					firstByte = false
				}
				// 把上游内容写入客户端响应体，并刷新缓冲区，如果写入失败，则返回context.Canceled错误
				written, writeErr := w.Write(buffer[:read])
				result.ResponseBytes += int64(written)
				// Flush() 的作用是将缓冲区中的数据立即发送给客户端，而不是等待缓冲区满或请求结束。这样可以减少延迟，尤其是在流式响应或SSE场景下，确保客户端能够及时接收到数据。
				// 通过类型断言检查 ResponseWriter 是否实现了 http.Flusher 接口，如果实现了，则调用 Flush() 方法刷新缓冲区。
				// 如果没有实现 http.Flusher 接口，则说明无法刷新缓冲区，可能会导致客户端无法及时接收到数据。
				// 因此，在流式响应或SSE场景下，建议使用支持 http.Flusher 的 ResponseWriter，例如 http.ResponseWriter 或者自定义的 ResponseWriter。
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				if writeErr != nil {
					return result, context.Canceled
				}
			}
			// 如果读取上游响应体出错，且不是EOF，则返回读取错误；如果是EOF，则表示上游响应体已经读取完毕，返回结果
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					return result, nil
				}
				return result, readErr
			}
		}
	}

	// 如果不是流式响应或SSE，就进入普通响应处理，读取上游响应体，并写入客户端响应体，如果读取或写入失败，则返回相应的错误
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return Result{StatusCode: response.StatusCode, Result: classifyStatus(response.StatusCode)}, err
	}
	w.WriteHeader(response.StatusCode)
	written, writeErr := w.Write(data)
	return Result{StatusCode: response.StatusCode, Result: classifyStatus(response.StatusCode), ResponseStarted: true, ResponseBytes: int64(written), FirstByte: time.Since(started)}, writeErr
}

func requestIsStream(body []byte) bool {
	var payload struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &payload) == nil && payload.Stream
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// classifyStatus 根据HTTP状态码返回一个字符串，表示请求结果的分类。2xx表示成功，4xx表示客户端错误，5xx表示服务器错误。
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

// defer的作用是保证对应的函数资源释放