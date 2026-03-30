package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore handles all Redis operations for token management
type RedisStore struct {
	client *redis.Client
}

// NewRedisStore creates a new Redis store
func NewRedisStore(addr string) (*RedisStore, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		DB:           0,
		PoolSize:     20,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis connect failed: %w", err)
	}

	return &RedisStore{client: client}, nil
}

func (r *RedisStore) freshKey(tenantID string) string { return fmt.Sprintf("tokens:%s:fresh", tenantID) }
func (r *RedisStore) inUseKey(tenantID string) string { return fmt.Sprintf("tokens:%s:in_use", tenantID) }
func (r *RedisStore) usedKey(tenantID string) string  { return fmt.Sprintf("tokens:%s:used", tenantID) }

// PopFreshTokens pops up to `count` tokens from fresh queue and moves them to in_use.
// Uses LMOVE for atomic transfer in a single round-trip per token.
func (r *RedisStore) PopFreshTokens(ctx context.Context, tenantID string, count int) ([]TokenData, error) {
	tokens := make([]TokenData, 0, count)

	for i := 0; i < count; i++ {
		val, err := r.client.LMove(ctx, r.freshKey(tenantID), r.inUseKey(tenantID), "LEFT", "RIGHT").Result()
		if err == redis.Nil {
			break
		}
		if err != nil {
			return tokens, fmt.Errorf("lmove failed: %w", err)
		}

		var token TokenData
		if err := json.Unmarshal([]byte(val), &token); err != nil {
			log.Printf("[WARN] invalid token JSON in fresh queue: %v", err)
			continue
		}

		tokens = append(tokens, token)
	}

	return tokens, nil
}

// markExhaustedScript atomically finds a token by email in in_use, removes it,
// stamps exhausted_at, and appends it to used. Returns 1 on success, 0 if not found.
const markExhaustedScript = `
local vals = redis.call('LRANGE', KEYS[1], 0, -1)
for i, v in ipairs(vals) do
    local t = cjson.decode(v)
    if t.email == ARGV[1] then
        redis.call('LREM', KEYS[1], 1, v)
        t.exhausted_at = tonumber(ARGV[2])
        redis.call('RPUSH', KEYS[2], cjson.encode(t))
        return 1
    end
end
return 0
`

// MarkExhausted moves a token from in_use to used queue with the given exhausted timestamp.
// The operation is atomic via a Lua script to prevent duplicate entries under concurrency.
func (r *RedisStore) MarkExhausted(ctx context.Context, tenantID string, email string, at time.Time) error {
	script := redis.NewScript(markExhaustedScript)
	result, err := script.Run(
		ctx,
		r.client,
		[]string{r.inUseKey(tenantID), r.usedKey(tenantID)},
		email,
		at.Unix(),
	).Int()
	if err != nil {
		return fmt.Errorf("mark exhausted script failed: %w", err)
	}
	if result == 0 {
		return fmt.Errorf("token not found for email: %s", email)
	}
	return nil
}

// GetStats returns queue lengths for a tenant
func (r *RedisStore) GetStats(ctx context.Context, tenantID string) (StatsResponse, error) {
	fresh, err := r.client.LLen(ctx, r.freshKey(tenantID)).Result()
	if err != nil {
		return StatsResponse{}, fmt.Errorf("llen fresh failed: %w", err)
	}
	inUse, err := r.client.LLen(ctx, r.inUseKey(tenantID)).Result()
	if err != nil {
		return StatsResponse{}, fmt.Errorf("llen in_use failed: %w", err)
	}
	used, err := r.client.LLen(ctx, r.usedKey(tenantID)).Result()
	if err != nil {
		return StatsResponse{}, fmt.Errorf("llen used failed: %w", err)
	}

	return StatsResponse{
		TenantID: tenantID,
		Fresh:    fresh,
		InUse:    inUse,
		Used:     used,
	}, nil
}

// GetAllTenants scans Redis for all tenant keys and returns unique tenant IDs
func (r *RedisStore) GetAllTenants(ctx context.Context) ([]string, error) {
	tenants := make(map[string]bool)

	iter := r.client.Scan(ctx, 0, "tokens:*:fresh", 100).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		start := len("tokens:")
		end := len(key) - len(":fresh")
		if start < end {
			tenants[key[start:end]] = true
		}
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("scan failed: %w", err)
	}

	result := make([]string, 0, len(tenants))
	for t := range tenants {
		result = append(result, t)
	}
	return result, nil
}

// Close closes the Redis connection
func (r *RedisStore) Close() error {
	return r.client.Close()
}
