package proxy

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDecodeObservedUpstreamQuota(t *testing.T) {
	now := time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)
	limit := `{"limitId":"codex","limitName":null,"primary":{"usedPercent":30,"windowDurationMins":300,"resetsAt":1794200000},"secondary":null,"rateLimitReachedType":null}`
	valid := `{"id":6,"result":{"rateLimits":` + limit + `,"rateLimitsByLimitId":{"codex":` + limit + `},"rateLimitResetCredits":null},"observed_at":"2026-11-15T00:59:59.000004Z"}`
	quota, err := DecodeObservedUpstreamQuota([]byte(valid), now)
	if err != nil || quota.ObservedAt == nil || !quota.ObservedAt.Equal(time.Date(2026, 11, 15, 0, 59, 59, 4000, time.UTC)) ||
		quota.Result.RateLimits.Primary == nil || quota.Result.RateLimits.Primary.UsedPercent != 30 {
		t.Fatalf("quota=%+v err=%v", quota, err)
	}
	for name, body := range map[string]string{
		"no observation time": strings.Replace(valid, `,"observed_at":"2026-11-15T00:59:59.000004Z"`, ``, 1),
		"future observation":  strings.Replace(valid, `2026-11-15T00:59:59`, `2026-11-15T01:06:00`, 1),
		"unknown field":       strings.Replace(valid, `{"id":6`, `{"extra":1,"id":6`, 1),
		"duplicate key":       strings.Replace(valid, `{"id":6`, `{"id":6,"id":6`, 1),
		"two values":          valid + `{}`,
		"mismatched primary":  strings.Replace(valid, `"usedPercent":30`, `"usedPercent":31`, 1),
		"anthropic limit":     strings.ReplaceAll(valid, `codex`, `anthropic`),
		"not json":            `quota`,
	} {
		if _, err := DecodeObservedUpstreamQuota([]byte(body), now); !errors.Is(err, ErrInvalidObservedQuota) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
