//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func newMiniRedisCache(t *testing.T) (*billingCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return &billingCache{rdb: rdb}, mr
}

func TestUserPlatformQuotaCache_GetMissReturnsNotFound(t *testing.T) {
	c, _ := newMiniRedisCache(t)
	entry, ok, err := c.GetUserPlatformQuotaCache(context.Background(), 1, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if ok || entry != nil {
		t.Errorf("expected miss, got ok=%v entry=%v", ok, entry)
	}
}

func TestUserPlatformQuotaCache_SetThenGet(t *testing.T) {
	c, _ := newMiniRedisCache(t)
	ctx := context.Background()
	dailyLimit := 20.0
	ts := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	in := &service.UserPlatformQuotaCacheEntry{
		DailyUsageUSD:    1.5,
		WeeklyUsageUSD:   3.0,
		MonthlyUsageUSD:  10.0,
		Version:          7,
		SchemaVersion:    service.UserPlatformQuotaCacheSchemaV1,
		DailyLimitUSD:    &dailyLimit,
		DailyWindowStart: &ts,
	}
	if err := c.SetUserPlatformQuotaCache(ctx, 1, "openai", in, time.Minute); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.GetUserPlatformQuotaCache(ctx, 1, "openai")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.DailyUsageUSD != 1.5 || got.WeeklyUsageUSD != 3.0 || got.MonthlyUsageUSD != 10.0 || got.Version != 7 {
		t.Errorf("got = %+v, want %+v", got, in)
	}
	if got.SchemaVersion != service.UserPlatformQuotaCacheSchemaV1 {
		t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, service.UserPlatformQuotaCacheSchemaV1)
	}
	if got.DailyLimitUSD == nil || *got.DailyLimitUSD != dailyLimit {
		t.Errorf("DailyLimitUSD = %v, want %v", got.DailyLimitUSD, dailyLimit)
	}
	if got.DailyWindowStart == nil || !got.DailyWindowStart.Equal(ts) {
		t.Errorf("DailyWindowStart = %v, want %v", got.DailyWindowStart, ts)
	}
}

func TestUserPlatformQuotaCache_NilLimitSetThenGet(t *testing.T) {
	c, _ := newMiniRedisCache(t)
	ctx := context.Background()
	in := &service.UserPlatformQuotaCacheEntry{
		DailyUsageUSD: 1.0,
		SchemaVersion: service.UserPlatformQuotaCacheSchemaV1,
		// DailyLimitUSD nil → 无限额
	}
	if err := c.SetUserPlatformQuotaCache(ctx, 1, "openai", in, time.Minute); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.GetUserPlatformQuotaCache(ctx, 1, "openai")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.DailyLimitUSD != nil {
		t.Errorf("DailyLimitUSD should be nil for unlimited, got %v", got.DailyLimitUSD)
	}
}

func TestUserPlatformQuotaCache_IncrMissIsNoop(t *testing.T) {
	c, _ := newMiniRedisCache(t)
	if err := c.IncrUserPlatformQuotaUsageCache(context.Background(), 1, "openai", 0.5, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	_, ok, _ := c.GetUserPlatformQuotaCache(context.Background(), 1, "openai")
	if ok {
		t.Error("expected key absent after no-op incr")
	}
}

func TestUserPlatformQuotaCache_IncrHitAccumulates(t *testing.T) {
	c, _ := newMiniRedisCache(t)
	ctx := context.Background()
	// SchemaVersion 必须显式设为 V1,否则 Lua 脚本会因 schema 不匹配而 return 0,跳过累加。
	_ = c.SetUserPlatformQuotaCache(ctx, 1, "openai", &service.UserPlatformQuotaCacheEntry{
		Version:       1,
		SchemaVersion: service.UserPlatformQuotaCacheSchemaV1,
	}, time.Minute)
	if err := c.IncrUserPlatformQuotaUsageCache(ctx, 1, "openai", 0.5, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := c.IncrUserPlatformQuotaUsageCache(ctx, 1, "openai", 0.25, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	got, _, _ := c.GetUserPlatformQuotaCache(ctx, 1, "openai")
	if got.DailyUsageUSD != 0.75 || got.WeeklyUsageUSD != 0.75 || got.MonthlyUsageUSD != 0.75 {
		t.Errorf("got %+v, want daily/weekly/monthly=0.75", got)
	}
	if got.Version != 3 {
		t.Errorf("version = %d, want 3 (initial 1 + 2 incr)", got.Version)
	}
}

func TestUserPlatformQuotaCache_Delete(t *testing.T) {
	c, _ := newMiniRedisCache(t)
	ctx := context.Background()
	_ = c.SetUserPlatformQuotaCache(ctx, 1, "openai", &service.UserPlatformQuotaCacheEntry{Version: 1}, time.Minute)
	if err := c.DeleteUserPlatformQuotaCache(ctx, 1, "openai"); err != nil {
		t.Fatal(err)
	}
	_, ok, _ := c.GetUserPlatformQuotaCache(ctx, 1, "openai")
	if ok {
		t.Error("expected miss after delete")
	}
}

func TestUserPlatformQuotaCache_ResetWeekly(t *testing.T) {
	c, mr := newMiniRedisCache(t)
	ctx := context.Background()
	oldStart := time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC)
	newStart := time.Date(2026, 8, 24, 1, 30, 0, 0, time.UTC)
	if err := c.SetUserPlatformQuotaCache(ctx, 7, "openai", &service.UserPlatformQuotaCacheEntry{
		WeeklyUsageUSD:    12.5,
		Version:           4,
		SchemaVersion:     service.UserPlatformQuotaCacheSchemaV1,
		WeeklyWindowStart: &oldStart,
	}, time.Minute); err != nil {
		t.Fatal(err)
	}

	updated, err := c.ResetUserPlatformWeeklyQuotaCache(ctx, 7, "openai", newStart, 10*time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	if !updated {
		t.Fatal("expected cache hit to be updated")
	}
	got, ok, err := c.GetUserPlatformQuotaCache(ctx, 7, "openai")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.WeeklyUsageUSD != 0 || got.WeeklyWindowStart == nil || !got.WeeklyWindowStart.Equal(newStart) {
		t.Fatalf("unexpected reset result: %+v", got)
	}
	if got.Version != 5 {
		t.Fatalf("version=%d, want 5", got.Version)
	}
	_ = mr
	dirty, err := c.rdb.SIsMember(ctx, userPlatformQuotaDirtySetKey(), userPlatformQuotaDirtyMember(7, "openai")).Result()
	if err != nil || !dirty {
		t.Fatal("reset cache key should be marked dirty")
	}

	updated, err = c.ResetUserPlatformWeeklyQuotaCache(ctx, 99, "openai", newStart, time.Minute, true)
	if err != nil || updated {
		t.Fatalf("cache miss: updated=%v err=%v", updated, err)
	}
}

func TestUserPlatformQuotaCache_ResetWeeklyPreservesUsageFromCurrentWindow(t *testing.T) {
	c, _ := newMiniRedisCache(t)
	ctx := context.Background()
	newStart := time.Date(2026, 8, 24, 1, 30, 0, 0, time.UTC)
	if err := c.SetUserPlatformQuotaCache(ctx, 8, "openai", &service.UserPlatformQuotaCacheEntry{
		WeeklyUsageUSD:    0.75,
		Version:           9,
		SchemaVersion:     service.UserPlatformQuotaCacheSchemaV1,
		WeeklyWindowStart: &newStart,
	}, time.Minute); err != nil {
		t.Fatal(err)
	}

	updated, err := c.ResetUserPlatformWeeklyQuotaCache(ctx, 8, "openai", newStart, time.Minute, true)
	if err != nil || !updated {
		t.Fatalf("updated=%v err=%v", updated, err)
	}
	got, ok, err := c.GetUserPlatformQuotaCache(ctx, 8, "openai")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.WeeklyUsageUSD != 0.75 || got.Version != 9 {
		t.Fatalf("current-window usage was overwritten: %+v", got)
	}
}
