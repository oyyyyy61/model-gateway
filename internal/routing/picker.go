// Package routing owns model instance selection, local concurrency limits and
// the health state used by the proxy and the background health checker.
package routing

import (
	"net/url"
	"sort"
	"sync"
)

// picker.go 负责实例选择、并发控制和健康状态；Proxy 和健康探测器共用同一个 Picker。

// Instance describes one routable model instance.
// Instance 描述一个可路由的模型实例。
type Instance struct {
	ID          string
	Model       string
	Endpoint    string
	Weight      int
	MaxInflight int
}

// Snapshot is a point in time view of one instance, safe to return to callers.
// Snapshot 是某一时刻的实例视图，用于指标和管理端点。
type Snapshot struct {
	ID          string
	Model       string
	Endpoint    string
	Weight      int
	MaxInflight int
	Inflight    int
	Healthy     bool
}

// state 是 Picker 内部状态：健康标记、并发信号量和平滑加权轮询的当前权重。
type state struct {
	instance      Instance
	healthy       bool
	inflight      chan struct{}
	currentWeight int
}

// Picker selects instances with smooth weighted round robin.
// Picker 使用平滑加权轮询选择实例；inflight channel 充当并发信号量。
type Picker struct {
	mu        sync.Mutex
	instances []*state
}

// NewPicker validates the configuration and drops unusable instances.
// NewPicker 校验 endpoint/权重/并发数，非法实例直接丢弃。
func NewPicker(instances []Instance) *Picker {
	states := make([]*state, 0, len(instances))
	for _, instance := range instances {
		// 校验 Endpoint、权重和最大并发数；不合法则跳过该实例。
		parsed, err := url.Parse(instance.Endpoint)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || instance.Weight <= 0 || instance.MaxInflight <= 0 {
			continue
		}
		states = append(states, &state{
			instance: instance,
			healthy:  true,
			// inflight 是一个带缓冲的 channel，容量为 MaxInflight；
			// 发送一个空 struct 表示占用并发槽位，接收表示释放。
			inflight: make(chan struct{}, instance.MaxInflight),
		})
	}
	return &Picker{instances: states}
}

// Acquire reserves capacity on a healthy instance of the requested model.
// Acquire 是 AcquireExcept 的便捷入口。
func (p *Picker) Acquire(model string) (Instance, func(), bool) {
	return p.AcquireExcept(model, nil)
}

// AcquireExcept is Acquire with an exclusion set, used by proxy failover to
// avoid retrying the instance that just failed.
// AcquireExcept 额外排除本次已尝试的实例，避免故障转移重复打同一个实例。
func (p *Picker) AcquireExcept(model string, exclude map[string]struct{}) (Instance, func(), bool) {
	// 上锁，防止并发请求同时修改 currentWeight。
	p.mu.Lock()
	defer p.mu.Unlock()

	var selected *state
	totalWeight := 0
	// 过滤模型不匹配、不健康、已尝试过、并发已满的实例。
	for _, candidate := range p.instances {
		if candidate.instance.Model != model || !candidate.healthy {
			continue
		}
		if _, skip := exclude[candidate.instance.ID]; skip {
			continue
		}
		if len(candidate.inflight) >= cap(candidate.inflight) {
			continue
		}
		// 平滑加权：每个候选累加自己的权重，选 currentWeight 最大的。
		candidate.currentWeight += candidate.instance.Weight
		totalWeight += candidate.instance.Weight
		if selected == nil || candidate.currentWeight > selected.currentWeight {
			selected = candidate
		}
	}
	if selected == nil {
		return Instance{}, nil, false
	}
	// 选中实例减去总权重，保证下次不会一直选中同一个实例。
	selected.currentWeight -= totalWeight
	// 占用一个并发槽位；返回的 release 必须由调用方执行。
	selected.inflight <- struct{}{}
	return selected.instance, func() { <-selected.inflight }, true
}

// SetHealth updates the routing state for one instance.
// SetHealth 由健康探测器和代理的连接失败调用。
func (p *Picker) SetHealth(instanceID string, healthy bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, candidate := range p.instances {
		if candidate.instance.ID == instanceID {
			candidate.healthy = healthy
		}
	}
}

// Healthy reports how many healthy instances serve the model.
// Healthy 返回指定模型的健康实例数。
func (p *Picker) Healthy(model string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, candidate := range p.instances {
		if candidate.instance.Model == model && candidate.healthy {
			count++
		}
	}
	return count
}

// HealthyCount reports how many healthy instances exist across all models.
// HealthyCount 返回所有模型的健康实例总数，供 /readyz 使用。
func (p *Picker) HealthyCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, candidate := range p.instances {
		if candidate.healthy {
			count++
		}
	}
	return count
}

// Models returns the distinct models in stable order.
// Models 返回去重并排序后的模型列表。
func (p *Picker) Models() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]struct{}, len(p.instances))
	models := make([]string, 0, len(p.instances))
	for _, candidate := range p.instances {
		if _, ok := seen[candidate.instance.Model]; ok {
			continue
		}
		seen[candidate.instance.Model] = struct{}{}
		models = append(models, candidate.instance.Model)
	}
	sort.Strings(models)
	return models
}

// Snapshot returns every configured instance, including unhealthy ones.
// Snapshot 返回所有实例（含不健康），按 ID 排序，便于稳定输出。
func (p *Picker) Snapshot() []Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]Snapshot, 0, len(p.instances))
	for _, candidate := range p.instances {
		result = append(result, Snapshot{
			ID:          candidate.instance.ID,
			Model:       candidate.instance.Model,
			Endpoint:    candidate.instance.Endpoint,
			Weight:      candidate.instance.Weight,
			MaxInflight: candidate.instance.MaxInflight,
			Inflight:    len(candidate.inflight),
			Healthy:     candidate.healthy,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
