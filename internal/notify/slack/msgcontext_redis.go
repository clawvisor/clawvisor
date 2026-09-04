package slack

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisMsgCtxPrefix = "clawvisor:slackmc:"

// redisMessageContextStore shares message context across replicas.
//
// This was the last piece of Slack state with no Redis implementation, and
// the decision bus makes that a live problem rather than a theoretical one:
// decisions are a durable LPUSH/BRPOP queue, so whichever instance pops a
// decision resolves it — routinely not the one that posted the prompt. With
// an in-memory store that instance finds nothing, and the resolved message
// silently loses both its attribution and its "View request details" button,
// because the detail it would stash lives in the context that just missed.
type redisMessageContextStore struct {
	rdb *redis.Client
}

// NewRedisMessageContextStore creates a Redis-backed message context store.
func NewRedisMessageContextStore(rdb *redis.Client) MessageContextStorer {
	return &redisMessageContextStore{rdb: rdb}
}

// redisMessageContext is the wire form. messageContext's own fields are
// unexported, so it cannot round-trip through encoding/json directly.
type redisMessageContext struct {
	Summary  string  `json:"summary"`
	Detail   []block `json:"detail"`
	Approver string  `json:"approver"`
}

func (s *redisMessageContextStore) Put(key string, mc messageContext, ttl time.Duration) {
	data, err := json.Marshal(redisMessageContext{
		Summary: mc.Summary, Detail: mc.Detail, Approver: mc.Approver,
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	s.rdb.Set(ctx, redisMsgCtxPrefix+key, data, ttl)
}

// SetApprover merges the clicker into an existing entry.
//
// Read-modify-write rather than atomic: the only concurrent writer would be a
// second click on a request already being resolved, and that click is
// rejected by the callback token before reaching here. Losing this race would
// cost an attribution line, not correctness.
func (s *redisMessageContextStore) SetApprover(key, approver string) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()

	fullKey := redisMsgCtxPrefix + key
	data, err := s.rdb.Get(ctx, fullKey).Bytes()
	if err != nil {
		return
	}
	var rc redisMessageContext
	if err := json.Unmarshal(data, &rc); err != nil {
		return
	}
	rc.Approver = approver

	updated, err := json.Marshal(rc)
	if err != nil {
		return
	}
	// KeepTTL: this is an update, not a fresh write, so it must not extend
	// the entry's life past the prompt it belongs to.
	s.rdb.Set(ctx, fullKey, updated, redis.KeepTTL)
}

// TakeForResolve returns the context and retires it in one step.
//
// GetDel is what makes "once only" hold across replicas: exactly one caller
// receives the value, so a duplicate resolution cannot render the detail or
// attribution twice.
func (s *redisMessageContextStore) TakeForResolve(key string) (messageContext, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()

	data, err := s.rdb.GetDel(ctx, redisMsgCtxPrefix+key).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			return messageContext{}, false
		}
		return messageContext{}, false
	}
	var rc redisMessageContext
	if err := json.Unmarshal(data, &rc); err != nil {
		return messageContext{}, false
	}
	return messageContext{
		Summary:  rc.Summary,
		Detail:   rc.Detail,
		Approver: rc.Approver,
	}, true
}

// Cleanup is a no-op — Redis key TTLs handle expiry.
func (s *redisMessageContextStore) Cleanup() {}

var _ MessageContextStorer = (*redisMessageContextStore)(nil)
