// routing.Picker 负责模型实例选择和本地并发控制。每个实例保存健康状态、权重和一个带容量的 channel，channel 充当并发信号量。
// 请求调用 Acquire 时，Picker 会过滤模型不匹配、不健康或并发已满的实例，再通过平滑加权算法选择实例，并返回一个 release 函数。
// Proxy 使用 defer release() 确保请求在成功、超时、取消和异常路径下都能释放并发额度。当前健康状态由连接错误触发，自动探测和恢复机制可以作为后续扩展。

package routing

import (
	"net/url"
	"sync"
)

// 模型实例描述
type Instance struct {
	ID          string
	Model       string
	Endpoint    string
	Weight      int
	// 实例最大并发数，超过该值的请求会被拒绝
	MaxInflight int
}

// 实例状态描述，包括实例信息、健康状态、当前并发数和当前权重
type state struct {
	instance      Instance
	healthy       bool
	inflight      chan struct{}
	currentWeight int
}

type Picker struct {
	// 用于保护实例状态的互斥锁，保证并发安全
	mu        sync.Mutex
	instances []*state
}

// 初始化Picker，传入模型实例列表，过滤掉不合法的实例，并为每个实例创建一个状态对象
func NewPicker(instances []Instance) *Picker {
	states := make([]*state, 0, len(instances))
	for _, instance := range instances {
		// 校验实例的Endpoint是否合法，权重和最大并发数是否大于0，如果不合法则跳过该实例
		parsed, err := url.Parse(instance.Endpoint)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || instance.Weight <= 0 || instance.MaxInflight <= 0 {
			continue
		}
		// inflight是一个带缓冲的通道，用于限制实例的最大并发数，缓冲区大小为MaxInflight。chan struct{}，容量为MaxInflight
		// 可以把它理解为MaxInflight个令牌桶，只有当有空闲的令牌时，才能处理新的请求。每次处理请求时，都会向inflight通道发送一个空结构体{}，表示占用一个并发槽位；处理完请求后，会从inflight通道接收一个空结构体{}，表示释放一个并发槽位。
		states = append(states, &state{instance: instance, healthy: true, inflight: make(chan struct{}, instance.MaxInflight)})
	}
	return &Picker{instances: states}
}

func (p *Picker) Acquire(model string) (Instance, func(), bool) {
	// 上锁 防止多请求同时进入更改currentWeight
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.instances) == 0 {
		return Instance{}, nil, false
	}
	var selected *state
	totalWeight := 0
	for _, candidate := range p.instances {
		if candidate.instance.Model != model || !candidate.healthy {
			continue
		}
		// 过滤并发已满的实例，如果当前实例的inflight通道已经满了，则跳过该实例，继续选择下一个实例
		if len(candidate.inflight) >= cap(candidate.inflight) {
			continue
		}
		// 加权选择算法，每个实例的currentWeight会累加它的权重，选择currentWeight最大的实例作为当前请求的目标实例。
		// 选择后，selected实例的currentWeight会减去所有健康实例的总权重，以保证下次选择时不会一直选择同一个实例。
		candidate.currentWeight += candidate.instance.Weight
		totalWeight += candidate.instance.Weight
		if selected == nil || candidate.currentWeight > selected.currentWeight {
			selected = candidate
		}
	}
	// 占用一个并发槽位，返回释放函数和选择结果，如果没有可用实例，则返回false
	if selected != nil {
		selected.currentWeight -= totalWeight
		selected.inflight <- struct{}{}
		return selected.instance, func() { <-selected.inflight }, true
	}
	return Instance{}, nil, false
}

func (p *Picker) SetHealth(instanceID string, healthy bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, candidate := range p.instances {
		if candidate.instance.ID == instanceID {
			candidate.healthy = healthy
		}
	}
}

// 统计指定模型的健康实例数量，返回健康实例的数量
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
