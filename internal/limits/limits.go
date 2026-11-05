// limits 包实现网关的本地准入控制：按 API Key 的每分钟令牌桶，
// 以及每日 token 预算。
//
// 状态保存在进程内，单副本足够，也让网关保持零外部依赖；多副本部署时，
// 可以用 Redis 实现替换 Limiter，方法签名不变。MySQL 审计表已经记录
// 相同的用量字段。
package limits

import (
	"sync"
	"time"
)

// Limiter 记录每个 Key 的请求令牌桶和每日 token 用量。
type Limiter struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets map[string]*bucket
	usage   map[string]*tokenCounter
}

type bucket struct {
	tokens float64
	last   time.Time
}

type tokenCounter struct {
	day    string
	tokens int64
}

// New 使用系统时钟创建一个空的 Limiter。
func New() *Limiter {
	return &Limiter{
		now:     time.Now,
		buckets: make(map[string]*bucket),
		usage:   make(map[string]*tokenCounter),
	}
}

// SetClock 替换时钟，仅用于需要确定性的测试。
func (l *Limiter) SetClock(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}

// AllowRequest 从 Key 的每分钟令牌桶中消费一个令牌。
//
// rpm <= 0 表示不限制请求。返回的时长是等到下一个令牌可用的时间，
// 用于 Retry-After 响应头。
func (l *Limiter) AllowRequest(key string, rpm int) (bool, time.Duration) {
	if rpm <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	current, ok := l.buckets[key]
	if !ok {
		current = &bucket{tokens: float64(rpm), last: now}
		l.buckets[key] = current
	}

	if elapsed := now.Sub(current.last).Seconds(); elapsed > 0 {
		rate := float64(rpm) / 60.0
		current.tokens += elapsed * rate
		if current.tokens > float64(rpm) {
			current.tokens = float64(rpm)
		}
		current.last = now
	}

	if current.tokens >= 1 {
		current.tokens--
		return true, 0
	}

	rate := float64(rpm) / 60.0
	wait := time.Duration((1-current.tokens)/rate*float64(time.Second)) + time.Millisecond
	return false, wait
}

// AddTokens 按 Key 当前 UTC 日期累计 token 用量。
func (l *Limiter) AddTokens(key string, tokens int64) {
	if tokens <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	day := l.now().UTC().Format("2006-01-02")
	counter, ok := l.usage[key]
	if !ok || counter.day != day {
		counter = &tokenCounter{day: day}
		l.usage[key] = counter
	}
	counter.tokens += tokens
}

// TokensUsed 返回 Key 在当前 UTC 日期已记录的 token 用量。
func (l *Limiter) TokensUsed(key string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	day := l.now().UTC().Format("2006-01-02")
	counter, ok := l.usage[key]
	if !ok || counter.day != day {
		return 0
	}
	return counter.tokens
}

// QuotaExceeded 判断 Key 是否已达到每日 token 预算。
// daily <= 0 表示不限制配额。
func (l *Limiter) QuotaExceeded(key string, daily int64) (bool, int64) {
	used := l.TokensUsed(key)
	if daily <= 0 {
		return false, used
	}
	return used >= daily, used
}
