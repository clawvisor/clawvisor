package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/redis/go-redis/v9"
)

const (
	redisScopeDriftPrefix       = "clawvisor:scope_drift:"
	redisScopeDriftDriftPrefix  = redisScopeDriftPrefix + "drift:"
	redisScopeDriftPreClearPref = redisScopeDriftPrefix + "preclear:"
	redisScopeDriftPendingPref  = redisScopeDriftPrefix + "pending_sub:"

	// Hash field names on the drift key. `data` holds JSON of the
	// immutable subset of ScopeDrift (everything except the three
	// mutable claim fields). The three mutable fields live as their
	// own hash fields so ClaimOption / SetOutcome / RollbackClaim can
	// mutate them with HGET/HSET rather than patching serialized JSON.
	redisScopeDriftFieldData    = "data"
	redisScopeDriftFieldChosen  = "chosen"
	redisScopeDriftFieldOutcome = "outcome"
	redisScopeDriftFieldNote    = "note"
)

// RedisScopeDriftRegistry persists drift records, pre-clears, and
// pending tool_result substitutions in Redis so a multi-replica
// lite-proxy deployment can mint state on one instance and resolve it
// on another. ClaimOption / SetOutcome / RollbackClaim each run under
// one Lua script so the (drift mutable fields, pre-clear) pair stays
// coherent under concurrent access.
type RedisScopeDriftRegistry struct {
	rdb *redis.Client
	ttl time.Duration
	now func() time.Time
}

// NewRedisScopeDriftRegistry returns a Redis-backed registry. ttl <= 0
// falls back to 10 minutes — same default as
// NewMemoryScopeDriftRegistry. Pending substitutions ignore ttl and
// use substitutionTTL (24h) instead.
func NewRedisScopeDriftRegistry(rdb *redis.Client, ttl time.Duration) *RedisScopeDriftRegistry {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &RedisScopeDriftRegistry{rdb: rdb, ttl: ttl, now: time.Now}
}

// scopeDriftImmutable is the JSON payload stored in the `data` hash
// field — everything on ScopeDrift except the three mutable claim
// fields, which live as separate hash fields. Using a private shape
// (rather than `type rawScopeDrift ScopeDrift`) keeps the wire format
// stable even if someone adds more mutable fields to ScopeDrift later
// — the immutable side here would still round-trip correctly.
type scopeDriftImmutable struct {
	ID             string                `json:"id"`
	UserID         string                `json:"user_id"`
	AgentID        string                `json:"agent_id"`
	ConversationID string                `json:"conversation_id"`
	Provider       string                `json:"provider"`
	ToolUse        json.RawMessage       `json:"tool_use"`
	Service        string                `json:"service"`
	Action         string                `json:"action"`
	Host           string                `json:"host"`
	Method         string                `json:"method"`
	Path           string                `json:"path"`
	TaskID         string                `json:"task_id"`
	TaskPurpose    string                `json:"task_purpose"`
	ExpectedUse    string                `json:"expected_use"`
	Source         ScopeDriftSource      `json:"source"`
	ReasonText     string                `json:"reason_text"`
	CreatedAt      time.Time             `json:"created_at"`
	ExpiresAt      time.Time             `json:"expires_at"`
}

func encodeDriftImmutable(d ScopeDrift) ([]byte, error) {
	toolUse, err := json.Marshal(d.ToolUse)
	if err != nil {
		return nil, fmt.Errorf("marshal tool_use: %w", err)
	}
	return json.Marshal(scopeDriftImmutable{
		ID:             d.ID,
		UserID:         d.UserID,
		AgentID:        d.AgentID,
		ConversationID: d.ConversationID,
		Provider:       string(d.Provider),
		ToolUse:        toolUse,
		Service:        d.Service,
		Action:         d.Action,
		Host:           d.Host,
		Method:         d.Method,
		Path:           d.Path,
		TaskID:         d.TaskID,
		TaskPurpose:    d.TaskPurpose,
		ExpectedUse:    d.ExpectedUse,
		Source:         d.Source,
		ReasonText:     d.ReasonText,
		CreatedAt:      d.CreatedAt,
		ExpiresAt:      d.ExpiresAt,
	})
}

func decodeDriftImmutable(raw []byte) (ScopeDrift, error) {
	var imm scopeDriftImmutable
	if err := json.Unmarshal(raw, &imm); err != nil {
		return ScopeDrift{}, err
	}
	d := ScopeDrift{
		ID:             imm.ID,
		UserID:         imm.UserID,
		AgentID:        imm.AgentID,
		ConversationID: imm.ConversationID,
		Service:        imm.Service,
		Action:         imm.Action,
		Host:           imm.Host,
		Method:         imm.Method,
		Path:           imm.Path,
		TaskID:         imm.TaskID,
		TaskPurpose:    imm.TaskPurpose,
		ExpectedUse:    imm.ExpectedUse,
		Source:         imm.Source,
		ReasonText:     imm.ReasonText,
		CreatedAt:      imm.CreatedAt,
		ExpiresAt:      imm.ExpiresAt,
	}
	d.Provider = conversation.Provider(imm.Provider)
	if len(imm.ToolUse) > 0 && string(imm.ToolUse) != "null" {
		if err := json.Unmarshal(imm.ToolUse, &d.ToolUse); err != nil {
			return ScopeDrift{}, fmt.Errorf("unmarshal tool_use: %w", err)
		}
	}
	return d, nil
}

func redisScopeDriftKey(driftID string) string {
	return redisScopeDriftDriftPrefix + driftID
}

func redisScopeDriftPreClearKey(agentID, fingerprint string) string {
	return redisScopeDriftPreClearPref + agentID + "|" + fingerprint
}

func redisScopeDriftPendingKey(key PendingSubstitutionKey) string {
	return redisScopeDriftPendingPref + key.AgentID + "|" + key.ConversationID + "|" + key.ToolUseID
}

// Register implements ScopeDriftRegistry.
func (r *RedisScopeDriftRegistry) Register(ctx context.Context, drift ScopeDrift) (ScopeDrift, error) {
	if r == nil || r.rdb == nil {
		return drift, errors.New("scope drift registry not configured")
	}
	now := r.now().UTC()
	if drift.ID == "" {
		id, err := newDriftID()
		if err != nil {
			return ScopeDrift{}, fmt.Errorf("mint drift id: %w", err)
		}
		drift.ID = id
	}
	if drift.CreatedAt.IsZero() {
		drift.CreatedAt = now
	}
	if drift.ExpiresAt.IsZero() {
		drift.ExpiresAt = now.Add(r.ttl)
	}
	immutable, err := encodeDriftImmutable(drift)
	if err != nil {
		return ScopeDrift{}, fmt.Errorf("marshal drift: %w", err)
	}
	key := redisScopeDriftKey(drift.ID)
	pipe := r.rdb.TxPipeline()
	pipe.HSet(ctx, key,
		redisScopeDriftFieldData, immutable,
		redisScopeDriftFieldChosen, string(drift.ChosenOption),
		redisScopeDriftFieldOutcome, string(drift.Outcome),
		redisScopeDriftFieldNote, drift.AgentNote,
	)
	pipe.PExpire(ctx, key, r.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return ScopeDrift{}, err
	}
	return drift, nil
}

// Get implements ScopeDriftRegistry. Redis-side TTL drives expiry, but
// ExpiresAt in the immutable payload is double-checked in case clocks
// drift between instances.
func (r *RedisScopeDriftRegistry) Get(ctx context.Context, driftID string) (ScopeDrift, error) {
	if r == nil || r.rdb == nil {
		return ScopeDrift{}, ErrDriftNotFound
	}
	return r.getDrift(ctx, driftID)
}

func (r *RedisScopeDriftRegistry) getDrift(ctx context.Context, driftID string) (ScopeDrift, error) {
	key := redisScopeDriftKey(driftID)
	fields, err := r.rdb.HMGet(ctx, key,
		redisScopeDriftFieldData,
		redisScopeDriftFieldChosen,
		redisScopeDriftFieldOutcome,
		redisScopeDriftFieldNote,
	).Result()
	if err != nil {
		return ScopeDrift{}, err
	}
	if len(fields) < 4 || fields[0] == nil {
		return ScopeDrift{}, ErrDriftNotFound
	}
	rawData, ok := fields[0].(string)
	if !ok {
		return ScopeDrift{}, ErrDriftNotFound
	}
	drift, err := decodeDriftImmutable([]byte(rawData))
	if err != nil {
		_ = r.rdb.Del(ctx, key).Err()
		return ScopeDrift{}, ErrDriftNotFound
	}
	if v, ok := fields[1].(string); ok {
		drift.ChosenOption = ScopeDriftOption(v)
	}
	if v, ok := fields[2].(string); ok {
		drift.Outcome = ScopeDriftOutcome(v)
	}
	if v, ok := fields[3].(string); ok {
		drift.AgentNote = v
	}
	if !drift.ExpiresAt.IsZero() && r.now().UTC().After(drift.ExpiresAt) {
		_ = r.rdb.Del(ctx, key).Err()
		return ScopeDrift{}, ErrDriftNotFound
	}
	return drift, nil
}

// ClaimOption implements ScopeDriftRegistry. The Lua script reads the
// current `chosen` field; if non-empty it surfaces "already resolved"
// without mutating anything, else it writes all three claim fields in
// one atomic block. PTTL is untouched — claiming an option must not
// extend the 10-minute drift lifetime.
func (r *RedisScopeDriftRegistry) ClaimOption(ctx context.Context, driftID string, option ScopeDriftOption, agentNote string) (ScopeDrift, error) {
	if r == nil || r.rdb == nil {
		return ScopeDrift{}, ErrDriftNotFound
	}
	key := redisScopeDriftKey(driftID)
	res, err := redisScopeDriftClaimOptionScript.Run(ctx, r.rdb, []string{key},
		string(option), string(ScopeDriftOutcomePending), agentNote,
	).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return ScopeDrift{}, ErrDriftNotFound
		}
		// The script returns a Redis error when the key is missing.
		if errors.Is(err, errRedisDriftNotFound) || err.Error() == "drift not found" {
			return ScopeDrift{}, ErrDriftNotFound
		}
		return ScopeDrift{}, err
	}
	status, _ := res.(int64)
	current, getErr := r.getDrift(ctx, driftID)
	if getErr != nil {
		return ScopeDrift{}, getErr
	}
	if status == 1 {
		return current, ErrDriftAlreadyResolved
	}
	return current, nil
}

// SetOutcome implements ScopeDriftRegistry. On Succeeded the script
// also mints the pre-clear keyed by (AgentID, fingerprint) with the
// same remaining PTTL as the drift, so the pre-clear and drift expire
// together — matching the memory impl's invariant.
func (r *RedisScopeDriftRegistry) SetOutcome(ctx context.Context, driftID string, outcome ScopeDriftOutcome) error {
	if r == nil || r.rdb == nil {
		return ErrDriftNotFound
	}
	current, err := r.getDrift(ctx, driftID)
	if err != nil {
		return err
	}
	driftKey := redisScopeDriftKey(driftID)
	preClearKey := redisScopeDriftPreClearKey(current.AgentID, current.Fingerprint())
	mintPreClear := "0"
	if outcome == ScopeDriftOutcomeSucceeded {
		mintPreClear = "1"
	}
	_, err = redisScopeDriftSetOutcomeScript.Run(ctx, r.rdb, []string{driftKey, preClearKey},
		string(outcome), mintPreClear, driftID,
	).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) || err.Error() == "drift not found" {
			return ErrDriftNotFound
		}
		return err
	}
	return nil
}

// RollbackClaim implements ScopeDriftRegistry. The script resets the
// three claim fields AND deletes the pre-clear key in one atomic
// block — the lockstep invariant scope_drift_registry.go:374-397
// calls out.
func (r *RedisScopeDriftRegistry) RollbackClaim(ctx context.Context, driftID string) error {
	if r == nil || r.rdb == nil {
		return ErrDriftNotFound
	}
	current, err := r.getDrift(ctx, driftID)
	if err != nil {
		return err
	}
	driftKey := redisScopeDriftKey(driftID)
	preClearKey := redisScopeDriftPreClearKey(current.AgentID, current.Fingerprint())
	_, err = redisScopeDriftRollbackClaimScript.Run(ctx, r.rdb, []string{driftKey, preClearKey}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) || err.Error() == "drift not found" {
			return ErrDriftNotFound
		}
		return err
	}
	return nil
}

// LookupPreClear implements ScopeDriftRegistry. GETDEL gives atomic
// one-shot consume of the pre-clear; the follow-up Get on the drift
// catches the rare case where the drift expired between the
// SetOutcome that minted the pre-clear and this lookup. If the drift
// is missing, the pre-clear is already deleted by the GETDEL so no
// extra cleanup is needed.
func (r *RedisScopeDriftRegistry) LookupPreClear(ctx context.Context, agentID, fingerprint string) (string, bool) {
	if r == nil || r.rdb == nil {
		return "", false
	}
	preClearKey := redisScopeDriftPreClearKey(agentID, fingerprint)
	raw, err := r.rdb.GetDel(ctx, preClearKey).Result()
	if err != nil {
		return "", false
	}
	driftID := raw
	if driftID == "" {
		return "", false
	}
	if _, err := r.getDrift(ctx, driftID); err != nil {
		return "", false
	}
	return driftID, true
}

// RegisterPendingSubstitution implements SubstitutionRegistry.
func (r *RedisScopeDriftRegistry) RegisterPendingSubstitution(ctx context.Context, key PendingSubstitutionKey, value PendingSubstitution) error {
	if r == nil || r.rdb == nil {
		return errors.New("scope drift registry not configured")
	}
	if key.AgentID == "" || key.ToolUseID == "" {
		return errors.New("pending substitution requires agent_id and tool_use_id")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal pending substitution: %w", err)
	}
	return r.rdb.Set(ctx, redisScopeDriftPendingKey(key), raw, substitutionTTL).Err()
}

// LookupPendingSubstitution implements SubstitutionRegistry. Does NOT
// consume the entry — restoration of the assistant turn must work on
// every future inbound while the substitution is live.
func (r *RedisScopeDriftRegistry) LookupPendingSubstitution(ctx context.Context, key PendingSubstitutionKey) (PendingSubstitution, bool) {
	if r == nil || r.rdb == nil {
		return PendingSubstitution{}, false
	}
	raw, err := r.rdb.Get(ctx, redisScopeDriftPendingKey(key)).Bytes()
	if err != nil {
		return PendingSubstitution{}, false
	}
	var sub PendingSubstitution
	if err := json.Unmarshal(raw, &sub); err != nil {
		_ = r.rdb.Del(ctx, redisScopeDriftPendingKey(key)).Err()
		return PendingSubstitution{}, false
	}
	return sub, true
}

// DeletePendingSubstitution implements SubstitutionRegistry.
func (r *RedisScopeDriftRegistry) DeletePendingSubstitution(ctx context.Context, key PendingSubstitutionKey) {
	if r == nil || r.rdb == nil {
		return
	}
	_ = r.rdb.Del(ctx, redisScopeDriftPendingKey(key)).Err()
}

// errRedisDriftNotFound is the sentinel returned by the Lua scripts
// when EXISTS reports the drift key is missing. The Go side maps it
// back to ErrDriftNotFound.
var errRedisDriftNotFound = errors.New("drift not found")

// redisScopeDriftClaimOptionScript writes the three claim fields iff
// `chosen` is currently empty — one-shot test-and-set. Returns 0 on
// success, 1 on already-resolved.
var redisScopeDriftClaimOptionScript = redis.NewScript(`
local key = KEYS[1]
local chosen = ARGV[1]
local outcome = ARGV[2]
local note = ARGV[3]
if redis.call('EXISTS', key) == 0 then
  return redis.error_reply('drift not found')
end
local current = redis.call('HGET', key, 'chosen')
if current and current ~= '' then
  return 1
end
redis.call('HSET', key, 'chosen', chosen, 'outcome', outcome, 'note', note)
return 0
`)

// redisScopeDriftSetOutcomeScript updates the outcome field and,
// when ARGV[2]=="1", mints the pre-clear with the same remaining PTTL
// as the drift. PTTL on the drift is untouched.
var redisScopeDriftSetOutcomeScript = redis.NewScript(`
local drift_key = KEYS[1]
local preclear_key = KEYS[2]
local outcome = ARGV[1]
local mint_preclear = ARGV[2]
local drift_id = ARGV[3]
if redis.call('EXISTS', drift_key) == 0 then
  return redis.error_reply('drift not found')
end
redis.call('HSET', drift_key, 'outcome', outcome)
if mint_preclear == '1' then
  local ttl = redis.call('PTTL', drift_key)
  if ttl and ttl > 0 then
    redis.call('SET', preclear_key, drift_id, 'PX', ttl)
  else
    redis.call('SET', preclear_key, drift_id)
  end
end
return 1
`)

// redisScopeDriftRollbackClaimScript resets the three claim fields
// AND deletes the pre-clear key in one atomic block. A SetOutcome
// (Succeeded) → rollback sequence must not leave a stale pre-clear
// behind.
var redisScopeDriftRollbackClaimScript = redis.NewScript(`
local drift_key = KEYS[1]
local preclear_key = KEYS[2]
if redis.call('EXISTS', drift_key) == 0 then
  return redis.error_reply('drift not found')
end
redis.call('HSET', drift_key, 'chosen', '', 'outcome', '', 'note', '')
redis.call('DEL', preclear_key)
return 1
`)

var _ ScopeDriftRegistry = (*RedisScopeDriftRegistry)(nil)
