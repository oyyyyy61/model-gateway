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
var ErrUpstreamUnavailable = errors.New("upstream unavailable")

// ErrRequestTooLarge means the request body exceeded the configured limit.
var ErrRequestTooLarge = errors.New("request body too large")

// ProxyConfig wires a Proxy.
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
	body, err := io.ReadAll(io.LimitReader(r.Body, p.maxBodyBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("read request body: %w", err)
	}
	if int64(len(body)) > p.maxBodyBytes {
		return Result{}, ErrRequestTooLarge
	}
	stream := requestIsStream(body)

	ctx := r.Context()
	if p.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.requestTimeout)
		defer cancel()
	}

	tried := make(map[string]struct{}, p.maxAttempts)
	var lastErr error
	attempts := 0
	for attempts < p.maxAttempts {
		instance, release, ok := p.picker.AcquireExcept(model, tried)
		if !ok {
			break
		}
		attempts++
		tried[instance.ID] = struct{}{}

		result, retry, err := p.attempt(ctx, w, r, model, instance, body, stream, started, attempts < p.maxAttempts)
		release()
		result.Attempts = attempts

		if !retry {
			return result, err
		}
		lastErr = err
		if p.logger != nil {
			p.logger.Warn("upstream attempt failed, trying another instance", "model", model, "instance", instance.ID, "attempt", attempts, "error", err)
		}
	}

	if lastErr != nil {
		return Result{Attempts: attempts}, lastErr
	}
	return Result{Attempts: attempts}, ErrUpstreamUnavailable
}

func (p *Proxy) attempt(ctx context.Context, w http.ResponseWriter, r *http.Request, model string, instance routing.Instance, body []byte, stream bool, started time.Time, allowRetry bool) (Result, bool, error) {
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
	upstreamURL := strings.TrimRight(instance.Endpoint, "/") + upstreamPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	request, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL, bytes.NewReader(requestBody))
	if err != nil {
		return Result{InstanceID: instance.ID}, false, fmt.Errorf("create upstream request: %w", err)
	}
	request.Header = r.Header.Clone()
	request.Header.Del("Authorization")
	request.Header.Del("Content-Length")
	request.ContentLength = int64(len(requestBody))
	request.Header.Set("X-Forwarded-Request-ID", r.Header.Get("X-Request-ID"))

	response, err := p.client.Do(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Result{InstanceID: instance.ID}, false, context.DeadlineExceeded
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return Result{InstanceID: instance.ID}, false, context.Canceled
		}
		p.picker.SetHealth(instance.ID, false)
		if p.metrics != nil {
			p.metrics.UpstreamFailures.Inc(map[string]string{"instance": instance.ID})
		}
		return Result{InstanceID: instance.ID}, true, fmt.Errorf("upstream request: %w", err)
	}

	if allowRetry && isRetryableStatus(response.StatusCode) {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 32<<10))
		_ = response.Body.Close()
		if p.metrics != nil {
			p.metrics.UpstreamFailures.Inc(map[string]string{"instance": instance.ID})
		}
		return Result{InstanceID: instance.ID, StatusCode: response.StatusCode, Result: classifyStatus(response.StatusCode)}, true, nil
	}

	result, err := p.forward(w, response, stream, started)
	result.InstanceID = instance.ID
	return result, false, err
}

func (p *Proxy) forward(w http.ResponseWriter, response *http.Response, stream bool, started time.Time) (Result, error) {
	defer response.Body.Close()
	copyHeaders(w.Header(), response.Header)

	if stream || strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		result := Result{
			StatusCode:      response.StatusCode,
			Result:          classifyStatus(response.StatusCode),
			Stream:          true,
			ResponseStarted: true,
		}
		w.WriteHeader(response.StatusCode)
		tracker := &usage.StreamTracker{}
		buffer := make([]byte, 32*1024)
		firstByte := true
		for {
			read, readErr := response.Body.Read(buffer)
			if read > 0 {
				if firstByte {
					result.FirstByte = time.Since(started)
					firstByte = false
				}
				written, writeErr := w.Write(buffer[:read])
				result.ResponseBytes += int64(written)
				_, _ = tracker.Write(buffer[:read])
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				if writeErr != nil {
					result.Usage, _ = tracker.Usage()
					return result, context.Canceled
				}
			}
			if readErr != nil {
				result.Usage, _ = tracker.Usage()
				if errors.Is(readErr, io.EOF) {
					return result, nil
				}
				return result, readErr
			}
		}
	}

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

func requestIsStream(body []byte) bool {
	var payload struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &payload) == nil && payload.Stream
}

// ensureModel adds the model field when the caller selected it in the URL and
// left it out of the JSON body.
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
func (p *Proxy) MaxBodyBytes() int64 {
	return p.maxBodyBytes
}

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

func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// classifyStatus buckets an upstream status for logs and metrics.
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
