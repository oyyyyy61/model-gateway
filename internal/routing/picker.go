package routing

import (
	"net/url"
	"sync"
)

type Instance struct {
	ID          string
	Model       string
	Endpoint    string
	Weight      int
	MaxInflight int
}

type state struct {
	instance Instance
	healthy  bool
	inflight chan struct{}
	currentWeight int
}

type Picker struct {
	mu         sync.Mutex
	instances  []*state
}

func NewPicker(instances []Instance) *Picker {
	states := make([]*state, 0, len(instances))
	for _, instance := range instances {
		parsed, err := url.Parse(instance.Endpoint)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || instance.Weight <= 0 || instance.MaxInflight <= 0 {
			continue
		}
		states = append(states, &state{instance: instance, healthy: true, inflight: make(chan struct{}, instance.MaxInflight)})
	}
	return &Picker{instances: states}
}

func (p *Picker) Acquire(model string) (Instance, func(), bool) {
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
		if len(candidate.inflight) >= cap(candidate.inflight) {
			continue
		}
		candidate.currentWeight += candidate.instance.Weight
		totalWeight += candidate.instance.Weight
		if selected == nil || candidate.currentWeight > selected.currentWeight {
			selected = candidate
		}
	}
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
