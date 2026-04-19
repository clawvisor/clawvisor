package groupchat

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisBufferPrefix = "clawvisor:groupbuf:"

// RedisMessageBuffer stores group chat messages in Redis sorted sets (scored
// by Unix timestamp) for cross-instance access.
type RedisMessageBuffer struct {
	rdb      *redis.Client
	maxCount int
	maxAge   time.Duration
}

// NewRedisMessageBuffer creates a Redis-backed message buffer.
func NewRedisMessageBuffer(rdb *redis.Client, maxCount int, maxAge time.Duration) *RedisMessageBuffer {
	return &RedisMessageBuffer{rdb: rdb, maxCount: maxCount, maxAge: maxAge}
}

type bufferedMessageJSON struct {
	Text       string `json:"text"`
	SenderID   string `json:"sender_id"`
	SenderName string `json:"sender_name"`
	Timestamp  int64  `json:"ts"`
}

// Append adds a message to the group's buffer in Redis.
func (b *RedisMessageBuffer) Append(groupChatID string, msg BufferedMessage) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	data, err := json.Marshal(bufferedMessageJSON{
		Text:       msg.Text,
		SenderID:   msg.SenderID,
		SenderName: msg.SenderName,
		Timestamp:  msg.Timestamp.UnixMilli(),
	})
	if err != nil {
		return
	}

	key := redisBufferPrefix + groupChatID
	pipe := b.rdb.Pipeline()

	// Add with score = timestamp millis.
	pipe.ZAdd(ctx, key, redis.Z{
		Score:  float64(msg.Timestamp.UnixMilli()),
		Member: string(data),
	})

	// Trim to maxCount (keep highest scores = most recent).
	pipe.ZRemRangeByRank(ctx, key, 0, int64(-b.maxCount-1))

	// Set TTL to maxAge so stale groups are cleaned up automatically.
	pipe.Expire(ctx, key, b.maxAge+time.Minute)

	_, _ = pipe.Exec(ctx)
}

// Messages returns recent messages for the group within maxAge.
func (b *RedisMessageBuffer) Messages(groupChatID string) []BufferedMessage {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	key := redisBufferPrefix + groupChatID
	cutoff := time.Now().Add(-b.maxAge).UnixMilli()

	results, err := b.rdb.ZRangeByScore(ctx, key, &redis.ZRangeBy{
		Min: strconv.FormatInt(cutoff, 10),
		Max: "+inf",
	}).Result()
	if err != nil || len(results) == 0 {
		return nil
	}

	msgs := make([]BufferedMessage, 0, len(results))
	for _, raw := range results {
		var j bufferedMessageJSON
		if err := json.Unmarshal([]byte(raw), &j); err != nil {
			continue
		}
		msgs = append(msgs, BufferedMessage{
			Text:       j.Text,
			SenderID:   j.SenderID,
			SenderName: j.SenderName,
			Timestamp:  time.UnixMilli(j.Timestamp),
		})
	}
	return msgs
}

// RunCleanup is a no-op — Redis TTL and ZREMRANGEBYSCORE handle expiry.
func (b *RedisMessageBuffer) RunCleanup(ctx context.Context) {}

// MessagesForKeyPrefix scans Redis for all buffer keys starting with
// `prefix` (logical prefix, not including our redisBufferPrefix), then
// returns a copy of the non-stale entries per key.
func (b *RedisMessageBuffer) MessagesForKeyPrefix(prefix string) map[string][]BufferedMessage {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	match := redisBufferPrefix + prefix + "*"
	out := make(map[string][]BufferedMessage)
	var cursor uint64
	for {
		keys, next, err := b.rdb.Scan(ctx, cursor, match, 100).Result()
		if err != nil {
			return out
		}
		for _, k := range keys {
			logical := k[len(redisBufferPrefix):]
			msgs := b.Messages(logical)
			if len(msgs) > 0 {
				out[logical] = msgs
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return out
}
