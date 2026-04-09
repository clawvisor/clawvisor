package redis

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Connect parses a Redis URL and returns a connected client.
// The URL format follows the redis:// scheme (e.g. "redis://localhost:6379").
func Connect(ctx context.Context, url string) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parsing redis URL: %w", err)
	}
	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("connecting to redis: %w", err)
	}
	return client, nil
}
