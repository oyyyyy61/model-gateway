// health 包运行后台上游探测，保证路由表中的健康状态准确。
//
// 连接错误会立即把实例标记为不健康（快速失败）；恢复则要连续成功
// 达到配置次数，避免抖动的上游靠一次侥幸请求重新加入。
package health

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oyyyyy61/model-gateway/internal/metrics"
	"github.com/oyyyyy61/model-gateway/internal/routing"
)

// Status 是面向管理端的上游实例视图。
type Status struct {
	ID          string    `json:"id"`
	Model       string    `json:"model"`
	Endpoint    string    `json:"endpoint"`
	Healthy     bool      `json:"healthy"`
	Inflight    int       `json:"inflight"`
	LastChecked time.Time `json:"last_checked"`
	LastError   string    `json:"last_error,omitempty"`
	LastStatus  int       `json:"last_status_code,omitempty"`
	Failures    int       `json:"consecutive_failures"`
}

// Config 装配 Checker。
type Config struct {
	Picker    *routing.Picker
	Client    *http.Client
	Interval  time.Duration
	Path      string
	Failures  int
	Successes int
	Logger    *slog.Logger
	Metrics   *metrics.Metrics
}

// Checker 按固定间隔探测上游健康端点。
type Checker struct {
	picker    *routing.Picker
	client    *http.Client
	interval  time.Duration
	path      string
	failures  int
	successes int
	logger    *slog.Logger
	metrics   *metrics.Metrics

	mu     sync.Mutex
	states map[string]*probeState
}

type probeState struct {
	failures    int
	successes   int
	lastChecked time.Time
	lastError   string
	lastStatus  int
}

// New 创建 Checker；可选字段为空时使用安全默认值。
func New(cfg Config) *Checker {
	checker := &Checker{
		picker:    cfg.Picker,
		client:    cfg.Client,
		interval:  cfg.Interval,
		path:      cfg.Path,
		failures:  cfg.Failures,
		successes: cfg.Successes,
		logger:    cfg.Logger,
		metrics:   cfg.Metrics,
		states:    make(map[string]*probeState),
	}
	if checker.client == nil {
		checker.client = &http.Client{Timeout: 5 * time.Second}
	}
	if checker.path == "" {
		checker.path = "/healthz"
	}
	if checker.failures <= 0 {
		checker.failures = 1
	}
	if checker.successes <= 0 {
		checker.successes = 1
	}
	for _, snapshot := range checker.picker.Snapshot() {
		checker.states[snapshot.ID] = &probeState{}
	}
	return checker
}

// Run 先立即探测一次，之后按间隔探测，直到 ctx 结束。
func (c *Checker) Run(ctx context.Context) {
	if c.interval <= 0 {
		return
	}
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	c.CheckOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.CheckOnce(ctx)
		}
	}
}

// CheckOnce 并发探测所有已配置实例。
func (c *Checker) CheckOnce(ctx context.Context) {
	snapshots := c.picker.Snapshot()
	limit := make(chan struct{}, 8)
	var wait sync.WaitGroup
	for _, snapshot := range snapshots {
		wait.Add(1)
		limit <- struct{}{}
		go func(snapshot routing.Snapshot) {
			defer wait.Done()
			defer func() { <-limit }()
			c.check(ctx, snapshot)
		}(snapshot)
	}
	wait.Wait()
}

// Status 返回按实例 ID 排序的当前视图。
func (c *Checker) Status() []Status {
	snapshots := c.picker.Snapshot()
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]Status, 0, len(snapshots))
	for _, snapshot := range snapshots {
		status := Status{
			ID:       snapshot.ID,
			Model:    snapshot.Model,
			Endpoint: snapshot.Endpoint,
			Healthy:  snapshot.Healthy,
			Inflight: snapshot.Inflight,
		}
		if state, ok := c.states[snapshot.ID]; ok {
			status.LastChecked = state.lastChecked
			status.LastError = state.lastError
			status.LastStatus = state.lastStatus
			status.Failures = state.failures
		}
		result = append(result, status)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// HealthyCount 返回所有模型的健康实例总数。
func (c *Checker) HealthyCount() int {
	return c.picker.HealthyCount()
}

func (c *Checker) check(parent context.Context, snapshot routing.Snapshot) {
	timeout := c.interval
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	url := strings.TrimRight(snapshot.Endpoint, "/") + c.path
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		c.record(snapshot, false, 0, err)
		return
	}
	response, err := c.client.Do(request)
	if err != nil {
		c.record(snapshot, false, 0, err)
		return
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	healthy := response.StatusCode >= 200 && response.StatusCode < 300
	c.record(snapshot, healthy, response.StatusCode, nil)
}

func (c *Checker) record(snapshot routing.Snapshot, success bool, status int, probeErr error) {
	c.mu.Lock()
	state, ok := c.states[snapshot.ID]
	if !ok {
		state = &probeState{}
		c.states[snapshot.ID] = state
	}
	state.lastChecked = time.Now()
	state.lastStatus = status

	transition := false
	healthy := snapshot.Healthy
	if success {
		state.successes++
		state.failures = 0
		state.lastError = ""
		if !healthy && state.successes >= c.successes {
			healthy = true
			state.successes = 0
			transition = true
		}
	} else {
		state.failures++
		state.successes = 0
		if probeErr != nil {
			state.lastError = probeErr.Error()
		} else {
			state.lastError = http.StatusText(status)
			if state.lastError == "" {
				state.lastError = "unexpected status"
			}
		}
		if healthy && state.failures >= c.failures {
			healthy = false
			state.failures = 0
			transition = true
		}
	}
	lastError := state.lastError
	c.mu.Unlock()

	currentHealthy := snapshot.Healthy
	if transition {
		currentHealthy = healthy
		c.picker.SetHealth(snapshot.ID, healthy)
		if c.logger != nil {
			c.logger.Warn("upstream health changed", "instance", snapshot.ID, "model", snapshot.Model, "healthy", healthy, "last_error", lastError)
		}
	}

	if c.metrics != nil {
		result := "success"
		if !success {
			result = "failure"
		}
		c.metrics.HealthChecks.Inc(map[string]string{"instance": snapshot.ID, "result": result})
		c.metrics.UpstreamHealthy.Set(boolFloat(currentHealthy), map[string]string{"instance": snapshot.ID, "model": snapshot.Model})
	}
}

func boolFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
