// Package routing owns model instance selection, local concurrency limits and
// the health state used by the proxy and the background health checker.
package routing

import (
	"net/url"
	"sort"
	"sync"
)

// Instance describes one routable model instance.
type Instance struct {
	ID          string
	Model       string
	Endpoint    string
	Weight      int
	MaxInflight int
}

// Snapshot is a point in time view of one instance, safe to return to callers.
type Snapshot struct {
	ID          string
	Model       string
	Endpoint    string
	Weight      int
	MaxInflight int
	Inflight    int
	Healthy     bool
}

type state struct {
	instance      Instance
	healthy       bool
	inflight      chan struct{}
	currentWeight int
}

// Picker selects instances with smooth weighted round robin.
type Picker struct {
	mu        sync.Mutex
	instances []*state
}

// NewPicker validates the configuration and drops unusable instances.
func NewPicker(instances []Instance) *Picker {
	states := make([]*state, 0, len(instances))
	for _, instance := range instances {
		parsed, err := url.Parse(instance.Endpoint)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || instance.Weight <= 0 || instance.MaxInflight <= 0 {
			continue
		}
		states = append(states, &state{
			instance: instance,
			healthy:  true,
			inflight: make(chan struct{}, instance.MaxInflight),
		})
	}
	return &Picker{instances: states}
}

// Acquire reserves capacity on a healthy instance of the requested model.
func (p *Picker) Acquire(model string) (Instance, func(), bool) {
	return p.AcquireExcept(model, nil)
}

// AcquireExcept is Acquire with an exclusion set, used by proxy failover to
// avoid retrying the instance that just failed.
func (p *Picker) AcquireExcept(model string, exclude map[string]struct{}) (Instance, func(), bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var selected *state
	totalWeight := 0
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
		candidate.currentWeight += candidate.instance.Weight
		totalWeight += candidate.instance.Weight
		if selected == nil || candidate.currentWeight > selected.currentWeight {
			selected = candidate
		}
	}
	if selected == nil {
		return Instance{}, nil, false
	}
	selected.currentWeight -= totalWeight
	selected.inflight <- struct{}{}
	return selected.instance, func() { <-selected.inflight }, true
}

// SetHealth updates the routing state for one instance.
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
