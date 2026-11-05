// metrics 包暴露网关的 Prometheus 指标族，不引入外部客户端库。
//
// 指标族是固定的，因此基数可控，文本输出稳定且便于测试。
// 所有向量都支持并发使用。
package metrics

import (
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Metrics 汇总网关暴露的所有指标族。
type Metrics struct {
	Requests         *CounterVec
	Duration         *HistogramVec
	Tokens           *CounterVec
	Inflight         *GaugeVec
	UpstreamHealthy  *GaugeVec
	RateLimited      *CounterVec
	UpstreamFailures *CounterVec
	HealthChecks     *CounterVec
	BuildInfo        *GaugeVec
}

// New 构建完整的指标集合。
func New() *Metrics {
	return &Metrics{
		Requests:         NewCounterVec("gateway_requests_total", "Total gateway requests by model, tenant and outcome.", "model", "tenant", "status"),
		Duration:         NewHistogramVec("gateway_request_duration_seconds", "End-to-end gateway request duration in seconds.", defaultBuckets, "model"),
		Tokens:           NewCounterVec("gateway_tokens_total", "Token usage reported by upstream models.", "tenant", "model", "kind"),
		Inflight:         NewGaugeVec("gateway_inflight_requests", "Requests currently being proxied.", "model"),
		UpstreamHealthy:  NewGaugeVec("gateway_upstream_healthy", "1 when an upstream instance is considered healthy.", "instance", "model"),
		RateLimited:      NewCounterVec("gateway_rate_limited_total", "Requests rejected by gateway admission control.", "tenant", "reason"),
		UpstreamFailures: NewCounterVec("gateway_upstream_failures_total", "Upstream transport failures by instance.", "instance"),
		HealthChecks:     NewCounterVec("gateway_health_checks_total", "Background upstream health probe results.", "instance", "result"),
		BuildInfo:        NewGaugeVec("gateway_build_info", "Build information for the running gateway.", "version"),
	}
}

var defaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

// Render 返回 Prometheus 文本格式的指标。
func (m *Metrics) Render() string {
	var builder strings.Builder
	families := []interface{ render(*strings.Builder) }{
		m.Requests,
		m.Duration,
		m.Tokens,
		m.Inflight,
		m.UpstreamHealthy,
		m.RateLimited,
		m.UpstreamFailures,
		m.HealthChecks,
		m.BuildInfo,
	}
	for _, family := range families {
		family.render(&builder)
	}
	return builder.String()
}

// CounterVec 是单调递增的计数器族。
type CounterVec struct {
	name   string
	help   string
	labels []string
	mu     sync.Mutex
	values map[string]*counterSample
}

type counterSample struct {
	values []string
	value  float64
}

// NewCounterVec 创建一个空的计数器族。
func NewCounterVec(name, help string, labels ...string) *CounterVec {
	return &CounterVec{name: name, help: help, labels: labels, values: make(map[string]*counterSample)}
}

// Inc 给匹配的样本加一。
func (c *CounterVec) Inc(labels map[string]string) { c.Add(1, labels) }

// Add 给匹配的样本增加增量。
func (c *CounterVec) Add(delta float64, labels map[string]string) {
	key, values := labelKey(c.labels, labels)
	c.mu.Lock()
	defer c.mu.Unlock()
	sample, ok := c.values[key]
	if !ok {
		sample = &counterSample{values: values}
		c.values[key] = sample
	}
	sample.value += delta
}

func (c *CounterVec) render(builder *strings.Builder) {
	writeMeta(builder, c.name, c.help, "counter")
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, key := range sortedKeys(c.values) {
		sample := c.values[key]
		writeSample(builder, c.name, c.labels, sample.values, formatFloat(sample.value))
	}
}

// GaugeVec 是样本值可升可降的指标族。
type GaugeVec struct {
	name   string
	help   string
	labels []string
	mu     sync.Mutex
	values map[string]*gaugeSample
}

type gaugeSample struct {
	values []string
	value  float64
}

// NewGaugeVec 创建一个空的 Gauge 指标族。
func NewGaugeVec(name, help string, labels ...string) *GaugeVec {
	return &GaugeVec{name: name, help: help, labels: labels, values: make(map[string]*gaugeSample)}
}

// Set 替换匹配样本的值。
func (g *GaugeVec) Set(value float64, labels map[string]string) {
	key, values := labelKey(g.labels, labels)
	g.mu.Lock()
	defer g.mu.Unlock()
	sample, ok := g.values[key]
	if !ok {
		sample = &gaugeSample{values: values}
		g.values[key] = sample
	}
	sample.value = value
}

// Add 给匹配样本增加增量。
func (g *GaugeVec) Add(delta float64, labels map[string]string) {
	key, values := labelKey(g.labels, labels)
	g.mu.Lock()
	defer g.mu.Unlock()
	sample, ok := g.values[key]
	if !ok {
		sample = &gaugeSample{values: values}
		g.values[key] = sample
	}
	sample.value += delta
}

func (g *GaugeVec) render(builder *strings.Builder) {
	writeMeta(builder, g.name, g.help, "gauge")
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, key := range sortedKeys(g.values) {
		sample := g.values[key]
		writeSample(builder, g.name, g.labels, sample.values, formatFloat(sample.value))
	}
}

// HistogramVec 把观测值记录到固定分桶中。
type HistogramVec struct {
	name    string
	help    string
	labels  []string
	buckets []float64
	mu      sync.Mutex
	values  map[string]*histogramSample
}

type histogramSample struct {
	values []string
	counts []uint64
	sum    float64
	count  uint64
}

// NewHistogramVec 创建一个直方图族，分桶边界会先排序。
func NewHistogramVec(name, help string, buckets []float64, labels ...string) *HistogramVec {
	bounds := append([]float64(nil), buckets...)
	sort.Float64s(bounds)
	return &HistogramVec{name: name, help: help, labels: labels, buckets: bounds, values: make(map[string]*histogramSample)}
}

// Observe 记录一个观测值。
func (h *HistogramVec) Observe(value float64, labels map[string]string) {
	key, values := labelKey(h.labels, labels)
	h.mu.Lock()
	defer h.mu.Unlock()
	sample, ok := h.values[key]
	if !ok {
		sample = &histogramSample{values: values, counts: make([]uint64, len(h.buckets))}
		h.values[key] = sample
	}
	index := sort.SearchFloat64s(h.buckets, value)
	if index < len(sample.counts) {
		sample.counts[index]++
	}
	sample.sum += value
	sample.count++
}

func (h *HistogramVec) render(builder *strings.Builder) {
	writeMeta(builder, h.name, h.help, "histogram")
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, key := range sortedKeys(h.values) {
		sample := h.values[key]
		cumulative := uint64(0)
		for index, bound := range h.buckets {
			cumulative += sample.counts[index]
			writeSample(builder, h.name+"_bucket", appendCopy(h.labels, "le"), appendCopy(sample.values, formatFloat(bound)), strconv.FormatUint(cumulative, 10))
		}
		writeSample(builder, h.name+"_bucket", appendCopy(h.labels, "le"), appendCopy(sample.values, "+Inf"), strconv.FormatUint(sample.count, 10))
		writeSample(builder, h.name+"_sum", h.labels, sample.values, formatFloat(sample.sum))
		writeSample(builder, h.name+"_count", h.labels, sample.values, strconv.FormatUint(sample.count, 10))
	}
}

func labelKey(names []string, provided map[string]string) (string, []string) {
	values := make([]string, len(names))
	var builder strings.Builder
	for index, name := range names {
		value := provided[name]
		values[index] = value
		builder.WriteString(name)
		builder.WriteByte('=')
		builder.WriteString(value)
		builder.WriteByte(0)
	}
	return builder.String(), values
}

func writeMeta(builder *strings.Builder, name, help, kind string) {
	builder.WriteString("# HELP ")
	builder.WriteString(name)
	builder.WriteByte(' ')
	builder.WriteString(escapeHelp(help))
	builder.WriteByte('\n')
	builder.WriteString("# TYPE ")
	builder.WriteString(name)
	builder.WriteByte(' ')
	builder.WriteString(kind)
	builder.WriteByte('\n')
}

func writeSample(builder *strings.Builder, name string, labels, values []string, value string) {
	builder.WriteString(name)
	if len(labels) > 0 {
		builder.WriteByte('{')
		for index, label := range labels {
			if index > 0 {
				builder.WriteByte(',')
			}
			builder.WriteString(label)
			builder.WriteString(`="`)
			builder.WriteString(escapeLabel(values[index]))
			builder.WriteByte('"')
		}
		builder.WriteByte('}')
	}
	builder.WriteByte(' ')
	builder.WriteString(value)
	builder.WriteByte('\n')
}

func appendCopy(values []string, value string) []string {
	copied := make([]string, len(values)+1)
	copy(copied, values)
	copied[len(values)] = value
	return copied
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func escapeHelp(value string) string {
	return strings.NewReplacer("\\", `\\`, "\n", `\n`).Replace(value)
}

func escapeLabel(value string) string {
	return strings.NewReplacer("\\", `\\`, "\"", `\"`, "\n", `\n`).Replace(value)
}
