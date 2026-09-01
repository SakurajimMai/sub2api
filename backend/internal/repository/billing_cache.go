package repository

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	billingBalanceKeyPrefix   = "billing:balance:"
	billingSubKeyPrefix       = "billing:sub:"
	billingRateLimitKeyPrefix = "apikey:rate:"
	subCacheInvalidateChannel = "subscription:cache:invalidate"
	billingCacheTTL           = 5 * time.Minute
	billingCacheJitter        = 30 * time.Second
	rateLimitCacheTTL         = 7 * 24 * time.Hour // 7 days matches the longest window

	// Rate limit window durations — must match service.RateLimitWindow* constants.
	rateLimitWindow5h = 5 * time.Hour
	rateLimitWindow1d = 24 * time.Hour
	rateLimitWindow7d = 7 * 24 * time.Hour
)

// jitteredTTL 返回带随机抖动的 TTL，防止缓存雪崩
func jitteredTTL() time.Duration {
	// 只做“减法抖动”，确保实际 TTL 不会超过 billingCacheTTL（避免上界预期被打破）。
	if billingCacheJitter <= 0 {
		return billingCacheTTL
	}
	jitter := time.Duration(rand.IntN(int(billingCacheJitter)))
	return billingCacheTTL - jitter
}

// billingBalanceKey generates the Redis key for user balance cache.
func billingBalanceKey(userID int64) string {
	return fmt.Sprintf("%s%d", billingBalanceKeyPrefix, userID)
}

// billingSubKey generates the Redis key for subscription cache.
func billingSubKey(userID, groupID int64) string {
	return fmt.Sprintf("%s%d:%d", billingSubKeyPrefix, userID, groupID)
}

const (
	subFieldStatus       = "status"
	subFieldExpiresAt    = "expires_at"
	subFieldDailyUsage   = "daily_usage"
	subFieldWeeklyUsage  = "weekly_usage"
	subFieldMonthlyUsage = "monthly_usage"
	subFieldVersion      = "version"
)

// billingRateLimitKey generates the Redis key for API key rate limit cache.
func billingRateLimitKey(keyID int64) string {
	return fmt.Sprintf("%s%d", billingRateLimitKeyPrefix, keyID)
}

const (
	rateLimitFieldUsage5h  = "usage_5h"
	rateLimitFieldUsage1d  = "usage_1d"
	rateLimitFieldUsage7d  = "usage_7d"
	rateLimitFieldWindow5h = "window_5h"
	rateLimitFieldWindow1d = "window_1d"
	rateLimitFieldWindow7d = "window_7d"
)

var (
	deductBalanceScript = redis.NewScript(`
		local current = redis.call('GET', KEYS[1])
		if current == false then
			return 0
		end
		local newVal = tonumber(current) - tonumber(ARGV[1])
		redis.call('SET', KEYS[1], newVal)
		redis.call('EXPIRE', KEYS[1], ARGV[2])
		return 1
	`)

	updateSubUsageScript = redis.NewScript(`
		local exists = redis.call('EXISTS', KEYS[1])
		if exists == 0 then
			return 0
		end
		local cost = tonumber(ARGV[1])
		redis.call('HINCRBYFLOAT', KEYS[1], 'daily_usage', cost)
		redis.call('HINCRBYFLOAT', KEYS[1], 'weekly_usage', cost)
		redis.call('HINCRBYFLOAT', KEYS[1], 'monthly_usage', cost)
		redis.call('EXPIRE', KEYS[1], ARGV[2])
		return 1
	`)

	// updateRateLimitUsageScript atomically increments all three rate limit usage counters
	// with window expiration checking. If a window has expired, its usage is reset to cost
	// (instead of accumulated) and the window timestamp is updated, matching the DB-side
	// IncrementRateLimitUsage semantics.
	//
	// ARGV: [1]=cost, [2]=ttl_seconds, [3]=now_unix, [4]=window_5h_seconds, [5]=window_1d_seconds, [6]=window_7d_seconds
	updateRateLimitUsageScript = redis.NewScript(`
		local exists = redis.call('EXISTS', KEYS[1])
		if exists == 0 then
			return 0
		end
		local cost = tonumber(ARGV[1])
		local now = tonumber(ARGV[3])
		local win5h = tonumber(ARGV[4])
		local win1d = tonumber(ARGV[5])
		local win7d = tonumber(ARGV[6])

		-- Helper: check if window is expired and update usage + window accordingly
		-- Returns nothing, modifies the hash in-place.
		local function update_window(usage_field, window_field, window_duration)
			local w = tonumber(redis.call('HGET', KEYS[1], window_field) or 0)
			if w == 0 or (now - w) >= window_duration then
				-- Window expired or never started: reset usage to cost, start new window
				redis.call('HSET', KEYS[1], usage_field, tostring(cost))
				redis.call('HSET', KEYS[1], window_field, tostring(now))
			else
				-- Window still valid: accumulate
				redis.call('HINCRBYFLOAT', KEYS[1], usage_field, cost)
			end
		end

		update_window('usage_5h', 'window_5h', win5h)
		update_window('usage_1d', 'window_1d', win1d)
		update_window('usage_7d', 'window_7d', win7d)
		redis.call('EXPIRE', KEYS[1], ARGV[2])
		return 1
	`)
)

type billingCache struct {
	rdb *redis.Client
}

func NewBillingCache(rdb *redis.Client) service.BillingCache {
	return &billingCache{rdb: rdb}
}

func (c *billingCache) GetUserBalance(ctx context.Context, userID int64) (float64, error) {
	key := billingBalanceKey(userID)
	val, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(val, 64)
}

func (c *billingCache) SetUserBalance(ctx context.Context, userID int64, balance float64) error {
	key := billingBalanceKey(userID)
	return c.rdb.Set(ctx, key, balance, jitteredTTL()).Err()
}

func (c *billingCache) DeductUserBalance(ctx context.Context, userID int64, amount float64) error {
	key := billingBalanceKey(userID)
	_, err := deductBalanceScript.Run(ctx, c.rdb, []string{key}, amount, int(jitteredTTL().Seconds())).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		log.Printf("Warning: deduct balance cache failed for user %d: %v", userID, err)
		return err
	}
	return nil
}

func (c *billingCache) InvalidateUserBalance(ctx context.Context, userID int64) error {
	key := billingBalanceKey(userID)
	return c.rdb.Del(ctx, key).Err()
}

func (c *billingCache) GetSubscriptionCache(ctx context.Context, userID, groupID int64) (*service.SubscriptionCacheData, error) {
	key := billingSubKey(userID, groupID)
	result, err := c.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, redis.Nil
	}
	return c.parseSubscriptionCache(result)
}

func (c *billingCache) parseSubscriptionCache(data map[string]string) (*service.SubscriptionCacheData, error) {
	result := &service.SubscriptionCacheData{}

	result.Status = data[subFieldStatus]
	if result.Status == "" {
		return nil, errors.New("invalid cache: missing status")
	}

	if expiresStr, ok := data[subFieldExpiresAt]; ok {
		expiresAt, err := strconv.ParseInt(expiresStr, 10, 64)
		if err == nil {
			result.ExpiresAt = time.Unix(expiresAt, 0)
		}
	}

	if dailyStr, ok := data[subFieldDailyUsage]; ok {
		result.DailyUsage, _ = strconv.ParseFloat(dailyStr, 64)
	}

	if weeklyStr, ok := data[subFieldWeeklyUsage]; ok {
		result.WeeklyUsage, _ = strconv.ParseFloat(weeklyStr, 64)
	}

	if monthlyStr, ok := data[subFieldMonthlyUsage]; ok {
		result.MonthlyUsage, _ = strconv.ParseFloat(monthlyStr, 64)
	}

	if versionStr, ok := data[subFieldVersion]; ok {
		result.Version, _ = strconv.ParseInt(versionStr, 10, 64)
	}

	return result, nil
}

func (c *billingCache) SetSubscriptionCache(ctx context.Context, userID, groupID int64, data *service.SubscriptionCacheData) error {
	if data == nil {
		return nil
	}

	key := billingSubKey(userID, groupID)

	fields := map[string]any{
		subFieldStatus:       data.Status,
		subFieldExpiresAt:    data.ExpiresAt.Unix(),
		subFieldDailyUsage:   data.DailyUsage,
		subFieldWeeklyUsage:  data.WeeklyUsage,
		subFieldMonthlyUsage: data.MonthlyUsage,
		subFieldVersion:      data.Version,
	}

	pipe := c.rdb.Pipeline()
	pipe.HSet(ctx, key, fields)
	pipe.Expire(ctx, key, jitteredTTL())
	_, err := pipe.Exec(ctx)
	return err
}

func (c *billingCache) UpdateSubscriptionUsage(ctx context.Context, userID, groupID int64, cost float64) error {
	key := billingSubKey(userID, groupID)
	_, err := updateSubUsageScript.Run(ctx, c.rdb, []string{key}, cost, int(jitteredTTL().Seconds())).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		log.Printf("Warning: update subscription usage cache failed for user %d group %d: %v", userID, groupID, err)
		return err
	}
	return nil
}

func (c *billingCache) InvalidateSubscriptionCache(ctx context.Context, userID, groupID int64) error {
	key := billingSubKey(userID, groupID)
	return c.rdb.Del(ctx, key).Err()
}

func (c *billingCache) PublishSubscriptionCacheInvalidation(ctx context.Context, cacheKey string) error {
	return c.rdb.Publish(ctx, subCacheInvalidateChannel, cacheKey).Err()
}

func (c *billingCache) SubscribeSubscriptionCacheInvalidation(ctx context.Context, handler func(cacheKey string)) error {
	pubsub := c.rdb.Subscribe(ctx, subCacheInvalidateChannel)

	_, err := pubsub.Receive(ctx)
	if err != nil {
		_ = pubsub.Close()
		return fmt.Errorf("subscribe to subscription cache invalidation: %w", err)
	}

	go func() {
		defer func() {
			if err := pubsub.Close(); err != nil {
				log.Printf("Warning: failed to close subscription cache invalidation pubsub: %v", err)
			}
		}()

		ch := pubsub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				if msg != nil {
					handler(msg.Payload)
				}
			}
		}
	}()

	return nil
}

func (c *billingCache) GetAPIKeyRateLimit(ctx context.Context, keyID int64) (*service.APIKeyRateLimitCacheData, error) {
	key := billingRateLimitKey(keyID)
	result, err := c.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, redis.Nil
	}
	data := &service.APIKeyRateLimitCacheData{}
	if v, ok := result[rateLimitFieldUsage5h]; ok {
		data.Usage5h, _ = strconv.ParseFloat(v, 64)
	}
	if v, ok := result[rateLimitFieldUsage1d]; ok {
		data.Usage1d, _ = strconv.ParseFloat(v, 64)
	}
	if v, ok := result[rateLimitFieldUsage7d]; ok {
		data.Usage7d, _ = strconv.ParseFloat(v, 64)
	}
	if v, ok := result[rateLimitFieldWindow5h]; ok {
		data.Window5h, _ = strconv.ParseInt(v, 10, 64)
	}
	if v, ok := result[rateLimitFieldWindow1d]; ok {
		data.Window1d, _ = strconv.ParseInt(v, 10, 64)
	}
	if v, ok := result[rateLimitFieldWindow7d]; ok {
		data.Window7d, _ = strconv.ParseInt(v, 10, 64)
	}
	return data, nil
}

func (c *billingCache) SetAPIKeyRateLimit(ctx context.Context, keyID int64, data *service.APIKeyRateLimitCacheData) error {
	if data == nil {
		return nil
	}
	key := billingRateLimitKey(keyID)
	fields := map[string]any{
		rateLimitFieldUsage5h:  data.Usage5h,
		rateLimitFieldUsage1d:  data.Usage1d,
		rateLimitFieldUsage7d:  data.Usage7d,
		rateLimitFieldWindow5h: data.Window5h,
		rateLimitFieldWindow1d: data.Window1d,
		rateLimitFieldWindow7d: data.Window7d,
	}
	pipe := c.rdb.Pipeline()
	pipe.HSet(ctx, key, fields)
	pipe.Expire(ctx, key, rateLimitCacheTTL)
	_, err := pipe.Exec(ctx)
	return err
}

func (c *billingCache) UpdateAPIKeyRateLimitUsage(ctx context.Context, keyID int64, cost float64) error {
	key := billingRateLimitKey(keyID)
	now := time.Now().Unix()
	_, err := updateRateLimitUsageScript.Run(ctx, c.rdb, []string{key},
		cost,
		int(rateLimitCacheTTL.Seconds()),
		now,
		int(rateLimitWindow5h.Seconds()),
		int(rateLimitWindow1d.Seconds()),
		int(rateLimitWindow7d.Seconds()),
	).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		log.Printf("Warning: update rate limit usage cache failed for api key %d: %v", keyID, err)
		return err
	}
	return nil
}

func (c *billingCache) InvalidateAPIKeyRateLimit(ctx context.Context, keyID int64) error {
	key := billingRateLimitKey(keyID)
	return c.rdb.Del(ctx, key).Err()
}

// ============================================
// user × platform quota 缓存
// ============================================

// userPlatformQuotaCacheKey 构造 Redis key
func userPlatformQuotaCacheKey(userID int64, platform string) string {
	return fmt.Sprintf("billing:user_platform_quota:%d:%s", userID, platform)
}

// parseUserPlatformQuotaHash 将 Redis HGETALL 返回的 map[string]string 反序列化为
// *service.UserPlatformQuotaCacheEntry。空 map（key 不存在）返回 nil。
// GetUserPlatformQuotaCache 和 BatchGetUserPlatformQuotaCache 共用此函数，确保解析逻辑一致。
func parseUserPlatformQuotaHash(m map[string]string) *service.UserPlatformQuotaCacheEntry {
	if len(m) == 0 {
		return nil
	}
	parseFloat := func(s string) float64 {
		if s == "" {
			return 0
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			log.Printf("billing_cache: corrupt quota usage field %q (using 0): %v", s, err)
			return 0
		}
		return f
	}
	parseFloatPtr := func(s string) *float64 {
		if s == "" {
			return nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil
		}
		return &f
	}
	parseTimePtr := func(s string) *time.Time {
		if s == "" {
			return nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil
		}
		t := time.Unix(n, 0).UTC()
		return &t
	}
	parseInt64 := func(s string) int64 {
		n, _ := strconv.ParseInt(s, 10, 64)
		return n
	}
	return &service.UserPlatformQuotaCacheEntry{
		DailyUsageUSD:           parseFloat(m["daily_usage"]),
		WeeklyUsageUSD:          parseFloat(m["weekly_usage"]),
		MonthlyUsageUSD:         parseFloat(m["monthly_usage"]),
		Version:                 parseInt64(m["version"]),
		SchemaVersion:           parseInt64(m["schema_version"]),
		WeeklyGeneration:        parseInt64(m["weekly_generation"]),
		WeeklyPendingGeneration: parseInt64(m["weekly_pending_generation"]),
		WeeklyPendingUsageUSD:   parseFloat(m["weekly_pending_usage"]),
		WeeklyPendingEventID:    m["weekly_pending_event_id"],
		DailyLimitUSD:           parseFloatPtr(m["daily_limit"]),
		WeeklyLimitUSD:          parseFloatPtr(m["weekly_limit"]),
		MonthlyLimitUSD:         parseFloatPtr(m["monthly_limit"]),
		DailyWindowStart:        parseTimePtr(m["daily_window_start"]),
		WeeklyWindowStart:       parseTimePtr(m["weekly_window_start"]),
		MonthlyWindowStart:      parseTimePtr(m["monthly_window_start"]),
	}
}

func (c *billingCache) GetUserPlatformQuotaCache(ctx context.Context, userID int64, platform string) (*service.UserPlatformQuotaCacheEntry, bool, error) {
	key := userPlatformQuotaCacheKey(userID, platform)
	m, err := c.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, false, err
	}
	entry := parseUserPlatformQuotaHash(m)
	if entry == nil {
		// 空 map → key 不存在 → MISS
		return nil, false, nil
	}
	return entry, true, nil
}

const setUserPlatformQuotaCacheScript = `
local incoming_generation = tonumber(ARGV[6])
local fence_generation = tonumber(redis.call("HGET", KEYS[2], "generation") or 0)
local weekly_usage = ARGV[2]
local weekly_generation = incoming_generation
local weekly_window_start = ARGV[14]
local pending_generation = tonumber(ARGV[7])
local pending_usage = ARGV[8]
local pending_event_id = ARGV[9]
local existing_generation = tonumber(redis.call("HGET", KEYS[1], "weekly_generation") or 0)
local existing_pending_generation = tonumber(redis.call("HGET", KEYS[1], "weekly_pending_generation") or 0)
local existing_window_start = tonumber(redis.call("HGET", KEYS[1], "weekly_window_start") or 0)
local incoming_window_start = tonumber(ARGV[14]) or 0

if existing_generation > weekly_generation or
   (existing_generation == weekly_generation and existing_window_start > incoming_window_start) then
    weekly_generation = existing_generation
    weekly_usage = redis.call("HGET", KEYS[1], "weekly_usage") or weekly_usage
    weekly_window_start = redis.call("HGET", KEYS[1], "weekly_window_start") or weekly_window_start
elseif existing_generation == weekly_generation and existing_window_start == incoming_window_start then
    local existing_usage = tonumber(redis.call("HGET", KEYS[1], "weekly_usage") or 0)
    if existing_usage > tonumber(weekly_usage) then
        weekly_usage = existing_usage
    end
end
if existing_pending_generation > pending_generation then
    pending_generation = existing_pending_generation
    pending_usage = redis.call("HGET", KEYS[1], "weekly_pending_usage") or "0"
    pending_event_id = redis.call("HGET", KEYS[1], "weekly_pending_event_id") or ""
elseif existing_pending_generation == pending_generation and pending_generation > 0 then
    local existing_pending_usage = tonumber(redis.call("HGET", KEYS[1], "weekly_pending_usage") or 0)
    if existing_pending_usage > tonumber(pending_usage) then
        pending_usage = existing_pending_usage
    end
    local existing_pending_event_id = redis.call("HGET", KEYS[1], "weekly_pending_event_id") or ""
    if existing_pending_event_id ~= "" then
        pending_event_id = existing_pending_event_id
    end
end
if fence_generation > weekly_generation then
    if pending_generation == fence_generation then
        weekly_generation = existing_generation
        weekly_usage = redis.call("HGET", KEYS[1], "weekly_usage") or weekly_usage
        weekly_window_start = redis.call("HGET", KEYS[1], "weekly_window_start") or weekly_window_start
    else
        weekly_generation = fence_generation
        weekly_usage = "0"
        pending_generation = 0
        pending_usage = "0"
        pending_event_id = ""
    end
end

if pending_generation > fence_generation then
    redis.call("HSET", KEYS[2], "generation", pending_generation, "event_id", pending_event_id)
    redis.call("EXPIRE", KEYS[2], ARGV[16])
end

redis.call("HSET", KEYS[1],
    "daily_usage", ARGV[1],
    "weekly_usage", weekly_usage,
    "monthly_usage", ARGV[3],
    "version", ARGV[4],
    "schema_version", ARGV[5],
    "weekly_generation", weekly_generation,
    "weekly_pending_generation", pending_generation,
    "weekly_pending_usage", pending_usage,
    "weekly_pending_event_id", pending_event_id,
    "daily_limit", ARGV[10],
    "weekly_limit", ARGV[11],
    "monthly_limit", ARGV[12],
    "daily_window_start", ARGV[13],
    "weekly_window_start", weekly_window_start,
    "monthly_window_start", ARGV[15])
redis.call("EXPIRE", KEYS[1], ARGV[16])
return weekly_generation
`

func (c *billingCache) SetUserPlatformQuotaCache(ctx context.Context, userID int64, platform string, entry *service.UserPlatformQuotaCacheEntry, ttl time.Duration) error {
	if entry == nil {
		return nil
	}
	// 浮点可空字段：nil → 空字符串（读取时 parseFloatPtr 返回 nil，表示无限额）
	fmtFloatPtr := func(p *float64) string {
		if p == nil {
			return ""
		}
		return strconv.FormatFloat(*p, 'f', -1, 64)
	}
	// time.Time 可空字段：nil → 空字符串；有值 → unix 秒
	fmtTimePtr := func(p *time.Time) string {
		if p == nil {
			return ""
		}
		return strconv.FormatInt(p.Unix(), 10)
	}

	_, err := c.rdb.Eval(ctx, setUserPlatformQuotaCacheScript,
		[]string{userPlatformQuotaCacheKey(userID, platform), userPlatformQuotaGenerationFenceKey(userID, platform)},
		entry.DailyUsageUSD, entry.WeeklyUsageUSD, entry.MonthlyUsageUSD,
		entry.Version, entry.SchemaVersion, entry.WeeklyGeneration,
		entry.WeeklyPendingGeneration, entry.WeeklyPendingUsageUSD, entry.WeeklyPendingEventID,
		fmtFloatPtr(entry.DailyLimitUSD), fmtFloatPtr(entry.WeeklyLimitUSD), fmtFloatPtr(entry.MonthlyLimitUSD),
		fmtTimePtr(entry.DailyWindowStart), fmtTimePtr(entry.WeeklyWindowStart), fmtTimePtr(entry.MonthlyWindowStart),
		max(1, int(ttl.Seconds())),
	).Result()
	return err
}

func (c *billingCache) DeleteUserPlatformQuotaCache(ctx context.Context, userID int64, platform string) error {
	return c.rdb.Del(ctx, userPlatformQuotaCacheKey(userID, platform)).Err()
}

// updateUserPlatformQuotaUsageScript 缓存累加：EXISTS + schema_version 双重守卫。
// 旧版 entry（schema_version != ARGV[3]，包括缺字段的 0 值）不参与累加，由上层走 DB fallback 后
// SetCache 重建为新版 entry —— 若此处仍累加，上层覆盖时会丢失这部分增量，导致 Redis usage 比真实偏小。
// key 不存在同样跳过（由下次 SetCache 重建）。
// KEYS[1] = hash key
// KEYS[2] = 脏集 key（dirty set）
// ARGV[1] = cost (string float)
// ARGV[2] = ttl seconds
// ARGV[3] = expected schema_version (Go 侧 UserPlatformQuotaCacheSchemaV1)
// ARGV[4] = dirty set member（空串则不 SADD）
// ARGV[5] = 脏集兜底 TTL 秒
const updateUserPlatformQuotaUsageScript = `
if redis.call("EXISTS", KEYS[1]) == 0 then
    return 0
end
local ver = redis.call("HGET", KEYS[1], "schema_version")
if ver == false or tonumber(ver) ~= tonumber(ARGV[3]) then
    return 0
end
redis.call("HINCRBYFLOAT", KEYS[1], "daily_usage", ARGV[1])
redis.call("HINCRBYFLOAT", KEYS[1], "weekly_usage", ARGV[1])
redis.call("HINCRBYFLOAT", KEYS[1], "monthly_usage", ARGV[1])
redis.call("HINCRBY", KEYS[1], "version", 1)
redis.call("EXPIRE", KEYS[1], ARGV[2])
if ARGV[4] ~= "" then
    redis.call("SADD", KEYS[2], ARGV[4])
    redis.call("EXPIRE", KEYS[2], ARGV[5])
end
return 1
`

// resetUserPlatformWeeklyQuotaScript 在缓存结构有效时原子推进官方周窗口。
// 旧 schema 会被删除，由正常计费路径从 DB 重建，避免读取不完整字段。
const resetUserPlatformWeeklyQuotaScript = `
if redis.call("EXISTS", KEYS[1]) == 0 then
    return 0
end
local ver = redis.call("HGET", KEYS[1], "schema_version")
if ver == false or tonumber(ver) ~= tonumber(ARGV[3]) then
    redis.call("DEL", KEYS[1])
    return 0
end
local current_start = tonumber(redis.call("HGET", KEYS[1], "weekly_window_start") or 0)
local requested_start = tonumber(ARGV[1])
if current_start >= requested_start then
    redis.call("EXPIRE", KEYS[1], ARGV[2])
    return 1
end
redis.call("HSET", KEYS[1], "weekly_usage", "0", "weekly_window_start", ARGV[1])
redis.call("HINCRBY", KEYS[1], "version", 1)
redis.call("EXPIRE", KEYS[1], ARGV[2])
if ARGV[4] ~= "" then
    redis.call("SADD", KEYS[2], ARGV[4])
    redis.call("EXPIRE", KEYS[2], ARGV[5])
end
return 1
`

// userPlatformQuotaDirtySetKey 返回脏集（dirty set）的 Redis key。
// 使用与 userPlatformQuotaCacheKey 相同的前缀 "billing:"。
func userPlatformQuotaDirtySetKey() string { return "billing:" + "upq:dirty" }

func userPlatformQuotaGenerationFenceKey(userID int64, platform string) string {
	return "billing:upq:generation:" + userPlatformQuotaDirtyMember(userID, platform)
}

const prepareUserPlatformWeeklyQuotaResetScript = `
local fence_generation = tonumber(redis.call("HGET", KEYS[2], "generation") or 0)
local requested_generation = tonumber(ARGV[2])
if requested_generation > fence_generation then
    redis.call("HSET", KEYS[2], "generation", ARGV[2], "event_id", ARGV[1])
end
redis.call("EXPIRE", KEYS[2], ARGV[3])
if redis.call("EXISTS", KEYS[1]) == 0 then
    return 0
end
local schema = tonumber(redis.call("HGET", KEYS[1], "schema_version") or 0)
if schema ~= tonumber(ARGV[4]) then
    redis.call("DEL", KEYS[1])
    return 0
end
local current_generation = tonumber(redis.call("HGET", KEYS[1], "weekly_generation") or 0)
local pending_generation = tonumber(redis.call("HGET", KEYS[1], "weekly_pending_generation") or 0)
if current_generation >= requested_generation or pending_generation >= requested_generation then
    redis.call("EXPIRE", KEYS[1], ARGV[3])
    return 1
end
redis.call("HSET", KEYS[1],
    "weekly_pending_generation", ARGV[2],
    "weekly_pending_usage", "0",
    "weekly_pending_event_id", ARGV[1])
redis.call("EXPIRE", KEYS[1], ARGV[3])
return 1
`

const incrementUserPlatformQuotaUsageWithGenerationScript = `
if redis.call("EXISTS", KEYS[1]) == 0 then
    return -1
end
local schema = tonumber(redis.call("HGET", KEYS[1], "schema_version") or 0)
if schema ~= tonumber(ARGV[3]) then
    return -1
end
redis.call("HINCRBYFLOAT", KEYS[1], "daily_usage", ARGV[1])
redis.call("HINCRBYFLOAT", KEYS[1], "monthly_usage", ARGV[1])
local current_generation = tonumber(redis.call("HGET", KEYS[1], "weekly_generation") or 0)
local pending_generation = tonumber(redis.call("HGET", KEYS[1], "weekly_pending_generation") or 0)
local fence_generation = tonumber(redis.call("HGET", KEYS[3], "generation") or 0)
local applied_generation = current_generation
local effective_generation = current_generation
if pending_generation > effective_generation then
    effective_generation = pending_generation
end
if fence_generation > effective_generation then
    effective_generation = fence_generation
end
if effective_generation > current_generation then
    if pending_generation < effective_generation then
        redis.call("HSET", KEYS[1],
            "weekly_pending_generation", effective_generation,
            "weekly_pending_usage", "0",
            "weekly_pending_event_id", redis.call("HGET", KEYS[3], "event_id") or "")
    end
    redis.call("HINCRBYFLOAT", KEYS[1], "weekly_pending_usage", ARGV[1])
    applied_generation = effective_generation
else
    redis.call("HINCRBYFLOAT", KEYS[1], "weekly_usage", ARGV[1])
end
redis.call("HINCRBY", KEYS[1], "version", 1)
redis.call("EXPIRE", KEYS[1], ARGV[2])
if ARGV[4] ~= "" then
    redis.call("SADD", KEYS[2], ARGV[4])
    redis.call("EXPIRE", KEYS[2], ARGV[5])
end
return applied_generation
`

const finalizeUserPlatformWeeklyQuotaResetScript = `
local requested_generation = tonumber(ARGV[2])
local fence_generation = tonumber(redis.call("HGET", KEYS[3], "generation") or 0)
if requested_generation > fence_generation then
    redis.call("HSET", KEYS[3], "generation", ARGV[2], "event_id", ARGV[1])
end
redis.call("EXPIRE", KEYS[3], ARGV[5])
if redis.call("EXISTS", KEYS[1]) == 0 then
    return 0
end
local schema = tonumber(redis.call("HGET", KEYS[1], "schema_version") or 0)
if schema ~= tonumber(ARGV[6]) then
    redis.call("DEL", KEYS[1])
    return 0
end
local current_generation = tonumber(redis.call("HGET", KEYS[1], "weekly_generation") or 0)
if current_generation > requested_generation then
    redis.call("EXPIRE", KEYS[1], ARGV[5])
    return 1
end
local pending_generation = tonumber(redis.call("HGET", KEYS[1], "weekly_pending_generation") or 0)
if fence_generation > requested_generation or pending_generation > requested_generation then
    redis.call("EXPIRE", KEYS[1], ARGV[5])
    return 1
end
local pending_usage = tonumber(redis.call("HGET", KEYS[1], "weekly_usage") or 0)
if pending_generation == requested_generation then
    local staged_usage = tonumber(redis.call("HGET", KEYS[1], "weekly_pending_usage") or 0)
    if current_generation < requested_generation then
        pending_usage = staged_usage
    elseif staged_usage > pending_usage then
        pending_usage = staged_usage
    end
elseif current_generation < requested_generation then
    pending_usage = 0
end
redis.call("HSET", KEYS[1],
    "weekly_usage", pending_usage,
    "weekly_generation", ARGV[2],
    "weekly_window_start", ARGV[3],
    "weekly_pending_generation", "0",
    "weekly_pending_usage", "0",
    "weekly_pending_event_id", "")
redis.call("HINCRBY", KEYS[1], "version", 1)
redis.call("EXPIRE", KEYS[1], ARGV[5])
if ARGV[4] ~= "" then
    redis.call("SADD", KEYS[2], ARGV[4])
    redis.call("EXPIRE", KEYS[2], ARGV[7])
end
return 1
`

// userPlatformQuotaDirtyTTLSeconds 脏集兜底 TTL（秒）：初始 SADD（Lua）与 Readd 共用，
// 确保 flusher 长期停摆时脏集最终过期；正常运行因持续 SADD 不断续期。
const userPlatformQuotaDirtyTTLSeconds = 86400

// userPlatformQuotaDirtyMember 构造脏集成员字符串 "userID:platform"。
func userPlatformQuotaDirtyMember(userID int64, platform string) string {
	return strconv.FormatInt(userID, 10) + ":" + platform
}

func (c *billingCache) IncrUserPlatformQuotaUsageCache(ctx context.Context, userID int64, platform string, cost float64, ttl time.Duration, markDirty bool) error {
	member := ""
	if markDirty {
		member = userPlatformQuotaDirtyMember(userID, platform)
	}
	_, err := c.rdb.Eval(ctx, updateUserPlatformQuotaUsageScript,
		[]string{userPlatformQuotaCacheKey(userID, platform), userPlatformQuotaDirtySetKey()},
		strconv.FormatFloat(cost, 'f', -1, 64),
		int(ttl.Seconds()),
		service.UserPlatformQuotaCacheSchemaV1,
		member,
		userPlatformQuotaDirtyTTLSeconds,
	).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	return nil
}

func (c *billingCache) IncrUserPlatformQuotaUsageCacheWithGeneration(ctx context.Context, userID int64, platform string, cost float64, ttl time.Duration, markDirty bool) (int64, error) {
	member := ""
	if markDirty {
		member = userPlatformQuotaDirtyMember(userID, platform)
	}
	result, err := c.rdb.Eval(ctx, incrementUserPlatformQuotaUsageWithGenerationScript,
		[]string{userPlatformQuotaCacheKey(userID, platform), userPlatformQuotaDirtySetKey(), userPlatformQuotaGenerationFenceKey(userID, platform)},
		strconv.FormatFloat(cost, 'f', -1, 64), max(1, int(ttl.Seconds())), service.UserPlatformQuotaCacheSchemaV2,
		member, userPlatformQuotaDirtyTTLSeconds,
	).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		return 0, err
	}
	return result, nil
}

func (c *billingCache) PrepareUserPlatformWeeklyQuotaReset(ctx context.Context, userID int64, platform, eventID string, targetGeneration int64, ttl time.Duration) (bool, error) {
	result, err := c.rdb.Eval(ctx, prepareUserPlatformWeeklyQuotaResetScript,
		[]string{userPlatformQuotaCacheKey(userID, platform), userPlatformQuotaGenerationFenceKey(userID, platform)},
		eventID, targetGeneration, max(1, int(ttl.Seconds())), service.UserPlatformQuotaCacheSchemaV2,
	).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, err
	}
	return result == 1, nil
}

func (c *billingCache) FinalizeUserPlatformWeeklyQuotaReset(ctx context.Context, userID int64, platform, eventID string, targetGeneration int64, newStart time.Time, ttl time.Duration, markDirty bool) (bool, error) {
	member := ""
	if markDirty {
		member = userPlatformQuotaDirtyMember(userID, platform)
	}
	result, err := c.rdb.Eval(ctx, finalizeUserPlatformWeeklyQuotaResetScript,
		[]string{userPlatformQuotaCacheKey(userID, platform), userPlatformQuotaDirtySetKey(), userPlatformQuotaGenerationFenceKey(userID, platform)},
		eventID, targetGeneration, newStart.Unix(), member, max(1, int(ttl.Seconds())),
		service.UserPlatformQuotaCacheSchemaV2, userPlatformQuotaDirtyTTLSeconds,
	).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, err
	}
	return result == 1, nil
}

func (c *billingCache) ResetUserPlatformWeeklyQuotaCache(ctx context.Context, userID int64, platform string, newStart time.Time, ttl time.Duration, markDirty bool) (bool, error) {
	member := ""
	if markDirty {
		member = userPlatformQuotaDirtyMember(userID, platform)
	}
	ttlSeconds := int(ttl.Seconds())
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}
	result, err := c.rdb.Eval(ctx, resetUserPlatformWeeklyQuotaScript,
		[]string{userPlatformQuotaCacheKey(userID, platform), userPlatformQuotaDirtySetKey()},
		newStart.Unix(), ttlSeconds, service.UserPlatformQuotaCacheSchemaV1, member,
		userPlatformQuotaDirtyTTLSeconds,
	).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, err
	}
	return result == 1, nil
}

// parseUserPlatformQuotaDirtyMember 将脏集成员字符串 "userID:platform" 解析为
// service.UserPlatformQuotaKey。解析失败返回 ok=false。
func parseUserPlatformQuotaDirtyMember(m string) (service.UserPlatformQuotaKey, bool) {
	parts := strings.SplitN(m, ":", 2)
	if len(parts) != 2 {
		return service.UserPlatformQuotaKey{}, false
	}
	uid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return service.UserPlatformQuotaKey{}, false
	}
	return service.UserPlatformQuotaKey{UserID: uid, Platform: parts[1]}, true
}

// PopDirtyUserPlatformQuotaKeys 从脏集随机弹出最多 n 个 key。
// 脏集为空时返回 (nil, nil)。
func (c *billingCache) PopDirtyUserPlatformQuotaKeys(ctx context.Context, n int) ([]service.UserPlatformQuotaKey, error) {
	members, err := c.rdb.SPopN(ctx, userPlatformQuotaDirtySetKey(), int64(n)).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	keys := make([]service.UserPlatformQuotaKey, 0, len(members))
	for _, m := range members {
		k, ok := parseUserPlatformQuotaDirtyMember(m)
		if !ok {
			log.Printf("billing_cache: skipping invalid dirty member %q", m)
			continue
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// ReaddDirtyUserPlatformQuotaKeys 将 keys 重新加入脏集（flush 失败时回填）。
// 通过 pipeline 同时执行 SAdd + Expire，确保 Readd 后脏集具有兜底 TTL。
// 空切片时直接返回 nil。
func (c *billingCache) ReaddDirtyUserPlatformQuotaKeys(ctx context.Context, keys []service.UserPlatformQuotaKey) error {
	if len(keys) == 0 {
		return nil
	}
	dirtyKey := userPlatformQuotaDirtySetKey()
	members := make([]any, len(keys))
	for i, k := range keys {
		members[i] = userPlatformQuotaDirtyMember(k.UserID, k.Platform)
	}
	pipe := c.rdb.Pipeline()
	pipe.SAdd(ctx, dirtyKey, members...)
	pipe.Expire(ctx, dirtyKey, userPlatformQuotaDirtyTTLSeconds*time.Second)
	_, err := pipe.Exec(ctx)
	return err
}

// BatchGetUserPlatformQuotaCache 通过 Pipeline 批量 HGETALL 获取多个 user×platform 的
// quota cache。返回切片与 keys 顺序、长度对齐；MISS 或解析失败位置返回 nil。
func (c *billingCache) BatchGetUserPlatformQuotaCache(ctx context.Context, keys []service.UserPlatformQuotaKey) ([]*service.UserPlatformQuotaCacheEntry, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	pipe := c.rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(keys))
	for i, k := range keys {
		cmds[i] = pipe.HGetAll(ctx, userPlatformQuotaCacheKey(k.UserID, k.Platform))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	results := make([]*service.UserPlatformQuotaCacheEntry, len(keys))
	for i, cmd := range cmds {
		m, err := cmd.Result()
		if err != nil {
			if !errors.Is(err, redis.Nil) {
				log.Printf("billing_cache: BatchGet HGETALL cmd[%d] failed: %v (skip, self-heal)", i, err)
			}
			// 单个命令失败 → 对应位置 nil，继续
			continue
		}
		results[i] = parseUserPlatformQuotaHash(m)
	}
	return results, nil
}
