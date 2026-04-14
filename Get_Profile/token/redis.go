package token

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisLoader handles loading tokens from Redis and cleaning up dead tokens.
type RedisLoader struct {
	client *redis.Client
}

// NewRedisLoader connects to Redis at addr and verifies connectivity with a ping.
// Returns an error if Redis is unreachable.
func NewRedisLoader(addr string) (*RedisLoader, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		PoolSize:     10,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("cannot connect to Redis at %s: %w", addr, err)
	}

	return &RedisLoader{client: client}, nil
}

// LoadTokens reads all tokens from the Redis hash `redis-tokens-{queueSuffix}`.
// Each hash value must be in the format "email|refresh_token|tenant_id".
// Invalid entries are skipped with a warning log. Returns an error if no valid tokens are found.
func (r *RedisLoader) LoadTokens(queueSuffix string) ([]*TokenInfo, error) {
	key := "redis-tokens-" + queueSuffix

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	entries, err := r.client.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("hgetall %q: %w", key, err)
	}

	tokens := make([]*TokenInfo, 0, len(entries))

	for _, val := range entries {
		parts := strings.Split(val, "|")
		if len(parts) != 3 {
			slog.Warn("token/redis: skipping invalid entry",
				"key", key,
				"reason", "expected 3 pipe-separated fields",
				"got", len(parts),
			)
			continue
		}

		email := strings.TrimSpace(parts[0])
		refreshToken := strings.TrimSpace(parts[1])
		tenantID := strings.TrimSpace(parts[2])

		if email == "" || refreshToken == "" || tenantID == "" {
			slog.Warn("token/redis: skipping entry with empty fields",
				"key", key,
			)
			continue
		}

		tokens = append(tokens, &TokenInfo{
			Username:     email,
			RefreshToken: refreshToken,
			TenantID:     tenantID,
		})
	}

	if len(tokens) == 0 {
		return nil, fmt.Errorf("token/redis: no valid tokens found in %q", key)
	}

	slog.Info("token/redis: loaded tokens", "key", key, "count", len(tokens))

	return tokens, nil
}

// DeleteTokens removes the given emails from the Redis hash `redis-tokens-{queueSuffix}`.
// Errors are logged and not returned (best-effort deletion).
func (r *RedisLoader) DeleteTokens(queueSuffix string, emails []string) {
	if len(emails) == 0 {
		return
	}

	key := "redis-tokens-" + queueSuffix

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// HDEL accepts variadic fields; convert []string to []interface{}.
	fields := make([]string, len(emails))
	copy(fields, emails)

	if err := r.client.HDel(ctx, key, fields...).Err(); err != nil {
		slog.Error("token/redis: failed to delete dead tokens",
			"key", key,
			"count", len(emails),
			"err", err,
		)
	}
}

// StartCleanupWorker launches a background goroutine that drains deadChan,
// batches emails (flush at 50 items or every 5 seconds), and calls DeleteTokens.
// When ctx is cancelled the worker drains any remaining emails and flushes the final batch.
func (r *RedisLoader) StartCleanupWorker(ctx context.Context, queueSuffix string, deadChan <-chan string) {
	go func() {
		const batchSize = 50

		batch := make([]string, 0, batchSize)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		flush := func() {
			if len(batch) == 0 {
				return
			}
			r.DeleteTokens(queueSuffix, batch)
			batch = batch[:0]
		}

		for {
			select {
			case email, ok := <-deadChan:
				if !ok {
					// Channel closed — flush whatever remains and exit.
					flush()
					return
				}
				batch = append(batch, email)
				if len(batch) >= batchSize {
					flush()
				}

			case <-ticker.C:
				flush()

			case <-ctx.Done():
				// Drain remaining emails before exiting.
				for {
					select {
					case email, ok := <-deadChan:
						if !ok {
							flush()
							return
						}
						batch = append(batch, email)
						if len(batch) >= batchSize {
							flush()
						}
					default:
						flush()
						return
					}
				}
			}
		}
	}()
}

// Close shuts down the Redis client connection.
func (r *RedisLoader) Close() error {
	return r.client.Close()
}
