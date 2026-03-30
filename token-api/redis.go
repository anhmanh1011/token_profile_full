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

func freshKey(tenantID string) string { return fmt.Sprintf("tokens:%s:fresh", tenantID) }
func inUseKey(tenantID string) string { return fmt.Sprintf("tokens:%s:in_use", tenantID) }
func usedKey(tenantID string) string  { return fmt.Sprintf("tokens:%s:used", tenantID) }

// PopFreshTokens pops up to `count` tokens from fresh queue and moves them to in_use
func (r *RedisStore) PopFreshTokens(ctx context.Context, tenantID string, count int) ([]TokenData, error) {
	tokens := make([]TokenData, 0, count)

	for i := 0; i < count; i++ {
		val, err := r.client.LPop(ctx, freshKey(tenantID)).Result()
		if err == redis.Nil {
			break
		}
		if err != nil {
			return tokens, fmt.Errorf("lpop failed: %w", err)
		}

		var token TokenData
		if err := json.Unmarshal([]byte(val), &token); err != nil {
			log.Printf("[WARN] invalid token JSON in fresh queue: %v", err)
			continue
		}

		if err := r.client.RPush(ctx, inUseKey(tenantID), val).Err(); err != nil {
			log.Printf("[WARN] failed to push to in_use: %v", err)
		}

		tokens = append(tokens, token)
	}

	return tokens, nil
}

// MarkExhausted moves a token from in_use to used queue with exhausted_at timestamp
func (r *RedisStore) MarkExhausted(ctx context.Context, tenantID string, email string) error {
	vals, err := r.client.LRange(ctx, inUseKey(tenantID), 0, -1).Result()
	if err != nil {
		return fmt.Errorf("lrange failed: %w", err)
	}

	for _, val := range vals {
		var token TokenData
		if err := json.Unmarshal([]byte(val), &token); err != nil {
			continue
		}

		if token.Email == email {
			r.client.LRem(ctx, inUseKey(tenantID), 1, val)

			token.ExhaustedAt = TimeNow().Unix()
			updatedJSON, err := json.Marshal(token)
			if err != nil {
				return fmt.Errorf("marshal failed: %w", err)
			}
			r.client.RPush(ctx, usedKey(tenantID), string(updatedJSON))
			return nil
		}
	}

	return fmt.Errorf("token not found for email: %s", email)
}

// GetStats returns queue lengths for a tenant
func (r *RedisStore) GetStats(ctx context.Context, tenantID string) (StatsResponse, error) {
	fresh, err := r.client.LLen(ctx, freshKey(tenantID)).Result()
	if err != nil {
		return StatsResponse{}, err
	}
	inUse, err := r.client.LLen(ctx, inUseKey(tenantID)).Result()
	if err != nil {
		return StatsResponse{}, err
	}
	used, err := r.client.LLen(ctx, usedKey(tenantID)).Result()
	if err != nil {
		return StatsResponse{}, err
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
