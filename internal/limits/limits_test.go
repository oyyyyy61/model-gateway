package limits

import (
	"testing"
	"time"
)

func TestAllowRequestTokenBucket(t *testing.T) {
	now := time.Unix(0, 0)
	limiter := New()
	limiter.SetClock(func() time.Time { return now })

	for index := 0; index < 3; index++ {
		if allowed, _ := limiter.AllowRequest("key", 3); !allowed {
			t.Fatalf("expected burst request %d to be allowed", index)
		}
	}
	allowed, wait := limiter.AllowRequest("key", 3)
	if allowed {
		t.Fatal("expected the fourth request in the burst to be rejected")
	}
	if wait <= 0 {
		t.Fatalf("expected a positive retry delay, got %v", wait)
	}

	now = now.Add(20 * time.Second)
	if allowed, _ := limiter.AllowRequest("key", 3); !allowed {
		t.Fatal("expected a refilled token after 20 seconds")
	}
}

func TestUnlimitedRequestRate(t *testing.T) {
	limiter := New()
	for index := 0; index < 100; index++ {
		if allowed, _ := limiter.AllowRequest("key", 0); !allowed {
			t.Fatal("rpm=0 must disable request limiting")
		}
	}
}

func TestDailyTokenQuotaAndRollover(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	limiter := New()
	limiter.SetClock(func() time.Time { return now })

	limiter.AddTokens("key", 40)
	if used := limiter.TokensUsed("key"); used != 40 {
		t.Fatalf("expected 40 tokens, got %d", used)
	}
	if exceeded, used := limiter.QuotaExceeded("key", 100); exceeded || used != 40 {
		t.Fatalf("quota should not be exceeded yet, exceeded=%v used=%d", exceeded, used)
	}
	limiter.AddTokens("key", 70)
	if exceeded, used := limiter.QuotaExceeded("key", 100); !exceeded || used != 110 {
		t.Fatalf("expected quota exceeded at 110 tokens, exceeded=%v used=%d", exceeded, used)
	}

	now = now.Add(24 * time.Hour)
	if used := limiter.TokensUsed("key"); used != 0 {
		t.Fatalf("expected a fresh daily counter, got %d", used)
	}
	if exceeded, _ := limiter.QuotaExceeded("key", 100); exceeded {
		t.Fatal("quota must reset with the UTC day")
	}
}
