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

func NewProxy(cfg ProxyConfig) *Proxy {
	return &Proxy{picker: cfg.Picker, client: cfg.Client, requestTimeout: cfg.RequestTimeout, logger: cfg.Logger}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request, model string) (Result, error) {
	started := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		return Result{}, fmt.Errorf("read request body: %w", err)
	}
	stream := requestIsStream(body)
	instance, release, ok := p.picker.Acquire(model)
	if !ok {
		return Result{}, ErrUpstreamUnavailable
	}
	defer release()

	ctx := r.Context()
	if p.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.requestTimeout)
		defer cancel()
	}
	upstreamURL := strings.TrimRight(instance.Endpoint, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	request, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("create upstream request: %w", err)
	}
	request.Header = r.Header.Clone()
	request.Header.Del("Authorization")
	request.Header.Set("X-Forwarded-Request-ID", r.Header.Get("X-Request-ID"))

	response, err := p.client.Do(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Result{}, context.DeadlineExceeded
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return Result{}, context.Canceled
		}
		p.picker.SetHealth(instance.ID, false)
		return Result{}, fmt.Errorf("upstream request: %w", err)
	}
	defer response.Body.Close()

	copyHeaders(w.Header(), response.Header)
	if stream || strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		result := Result{StatusCode: response.StatusCode, Result: classifyStatus(response.StatusCode), Stream: true, ResponseStarted: true}
		w.WriteHeader(response.StatusCode)
		firstByte := true
		buffer := make([]byte, 32*1024)
		for {
			read, readErr := response.Body.Read(buffer)
			if read > 0 {
				if firstByte {
					result.FirstByte = time.Since(started)
					firstByte = false
				}
				written, writeErr := w.Write(buffer[:read])
				result.ResponseBytes += int64(written)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				if writeErr != nil {
					return result, context.Canceled
				}
			}
			if readErr != nil {
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
