package vault

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Vault entry kinds stored in the vault_entries.kind discriminator column.
const (
	// KindPush is a locally-encrypted pushed credential (the historical
	// behaviour). The stored ciphertext is the credential itself.
	KindPush = "push"
	// KindRef is an external-secret reference: the stored ciphertext is a
	// RefEnvelope (JSON) that names a secret in the customer's own store,
	// resolved to plaintext at Get time and never persisted.
	KindRef = "ref"
)

// Backend identifiers (exact wire strings) for reference resolvers.
const (
	BackendAWSSM     = "aws-sm"
	BackendGCPSM     = "gcp-sm"
	BackendHashiCorp = "hashicorp"
)

// refEnvelopeMarker is the value of the "$clawvisor_ref" field. It is a
// belt-and-suspenders integrity check: a decrypted ref row whose envelope
// lacks this marker is an error, never silently reinterpreted as a pushed
// value. Callers of the plain push path also reject inbound values that
// carry the marker (spoofing a ref via the value channel — gotcha #4).
const refEnvelopeMarker = 1

// refMarkerToken is the substring the push-value path scans for to reject a
// value that is trying to masquerade as a reference envelope.
const refMarkerToken = `"$clawvisor_ref"`

// RefEnvelope is the plaintext (pre-encryption) shape of a reference entry.
// It is AES-GCM sealed with the same rowAAD binding as a pushed value, so an
// ARN — which leaks account/region/naming — stays confidential at rest.
type RefEnvelope struct {
	Marker  int    `json:"$clawvisor_ref"`
	Backend string `json:"backend"`
	// ID is the backend-specific locator: an ARN (aws-sm), a full resource
	// name projects/{p}/secrets/{s} (gcp-sm; "/versions/N" optional, else
	// latest), or mount+path secret/data/foo (hashicorp KV v2).
	ID string `json:"id"`
	// JSONKey, when non-empty, selects a single field out of a JSON secret
	// payload. When empty, the raw fetched bytes are used verbatim.
	JSONKey string `json:"json_key,omitempty"`
}

// Resolver fetches the live plaintext for one backend. Implementations
// authenticate with AMBIENT cloud identity only (instance role / workload
// identity) — never credentials stored by Clawvisor.
type Resolver interface {
	Resolve(ctx context.Context, ref RefEnvelope) ([]byte, error)
}

// Reference resolution error classes. These are surfaced verbatim in API
// responses and terraform apply output. They MUST NOT carry secret content,
// the secret's JSON keys, its length, or any substring (structure-disclosure
// channel — see the spec's confused-deputy section).
var (
	// ErrRefTargetNotAllowed is returned at create time when a reference id
	// is not covered by the operator's vault.reference_allowlist (or the
	// allowlist is empty, i.e. references are disabled). The message is
	// deliberately generic and never echoes the allowlist.
	ErrRefTargetNotAllowed = errors.New("vault: reference target is not permitted by the server's reference allowlist")

	// ErrRefBackendUnknown is returned when a reference names a backend that
	// this build has no resolver for.
	ErrRefBackendUnknown = errors.New("vault: unknown reference backend")

	// ErrRefNotFound wraps ErrNotFound: the referenced secret does not exist
	// upstream.
	ErrRefNotFound = fmt.Errorf("%w: reference target does not exist in the external secret store", ErrNotFound)

	// ErrRefAccessDenied: Clawvisor's ambient identity lacks read access.
	ErrRefAccessDenied = errors.New("vault: Clawvisor's identity lacks read access to the referenced secret; grant secretsmanager:GetSecretValue / roles/secretmanager.secretAccessor / KV read")

	// ErrRefThrottled is the one retryable class (429 / throttling /
	// transient unavailability).
	ErrRefThrottled = errors.New("vault: reference resolution was throttled by the external secret store")

	// ErrRefKeyMissing: the configured json_key was not present in the
	// fetched payload. The message NEVER names the key or the available
	// keys — the operator holds the reference config; the server must not
	// confirm secret structure.
	ErrRefKeyMissing = errors.New("vault: the configured JSON key was not found in the referenced secret")

	// ErrRefMalformed: the decrypted ref row is not a valid envelope or its
	// marker/kind disagree. Indicates data corruption or tampering.
	ErrRefMalformed = errors.New("vault: reference entry is malformed")
)

// LooksLikeRefEnvelope reports whether a pushed value is trying to
// masquerade as a reference envelope. The credential API rejects such values
// on the plain `value` path so a member cannot smuggle a reference in via the
// member-allowed push channel (gotcha #4).
func LooksLikeRefEnvelope(value []byte) bool {
	return strings.Contains(string(value), refMarkerToken)
}

// refKindStore reads/writes the kind discriminator and the ref-envelope rows.
// Reference rows ALWAYS live in the DB (LocalVault-style), regardless of the
// configured push backend — so references work identically under `local` and
// `gcp` with one code path (design D2).
type refKindStore struct {
	crypto *LocalVault // AES-GCM helpers + db/driver, keyed by the master key
}

func (s refKindStore) ph(n int) string { return s.crypto.ph(n) }

// readKind returns the kind of the DB row for (userID, serviceID) and whether
// such a row exists. Under the `gcp` push backend, only ref rows live in the
// DB, so a missing row means "not a reference" and the caller falls through to
// the push backend.
func (s refKindStore) readKind(ctx context.Context, userID, serviceID string) (kind string, exists bool, err error) {
	q := fmt.Sprintf(
		`SELECT kind FROM vault_entries WHERE user_id = %s AND service_id = %s`,
		s.ph(1), s.ph(2),
	)
	err = s.crypto.db.QueryRowContext(ctx, q, userID, serviceID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("vault read kind: %w", err)
	}
	return kind, true, nil
}

// getEnvelope reads and decrypts the ref envelope for (userID, serviceID).
// It reuses the same rowAAD binding and lazy-AAD retry path as pushed values
// (gotcha #1: ref rows are AES-wrapped identically, so the legacy-AAD
// fallback must be exercised for them too).
func (s refKindStore) getEnvelope(ctx context.Context, userID, serviceID string) (RefEnvelope, error) {
	q := fmt.Sprintf(
		`SELECT encrypted, iv, auth_tag, kind FROM vault_entries WHERE user_id = %s AND service_id = %s`,
		s.ph(1), s.ph(2),
	)
	var encrypted, iv, authTag, kind string
	err := s.crypto.db.QueryRowContext(ctx, q, userID, serviceID).Scan(&encrypted, &iv, &authTag, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return RefEnvelope{}, ErrNotFound
	}
	if err != nil {
		return RefEnvelope{}, fmt.Errorf("vault get ref: %w", err)
	}
	if kind != KindRef {
		return RefEnvelope{}, ErrRefMalformed
	}
	aad := rowAAD(userID, serviceID)
	plaintext, decErr := s.crypto.decrypt(encrypted, iv, authTag, aad)
	if decErr != nil {
		// Lazy migration: rows sealed before AAD-binding used empty AAD.
		legacy, legacyErr := s.crypto.decrypt(encrypted, iv, authTag, nil)
		if legacyErr != nil {
			return RefEnvelope{}, decErr
		}
		plaintext = legacy
	}
	var env RefEnvelope
	if err := json.Unmarshal(plaintext, &env); err != nil {
		return RefEnvelope{}, ErrRefMalformed
	}
	if env.Marker != refEnvelopeMarker {
		return RefEnvelope{}, ErrRefMalformed
	}
	return env, nil
}

// putEnvelope encrypts and upserts the ref envelope, marking the row 'ref'.
func (s refKindStore) putEnvelope(ctx context.Context, userID, serviceID string, env RefEnvelope) error {
	env.Marker = refEnvelopeMarker
	plaintext, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("vault marshal ref: %w", err)
	}
	encrypted, iv, authTag, err := s.crypto.encrypt(plaintext, rowAAD(userID, serviceID))
	if err != nil {
		return fmt.Errorf("vault encrypt ref: %w", err)
	}
	id := uuid.New().String()
	var query string
	if s.crypto.driver == "postgres" {
		query = `
			INSERT INTO vault_entries (id, user_id, service_id, encrypted, iv, auth_tag, kind)
			VALUES ($1, $2, $3, $4, $5, $6, 'ref')
			ON CONFLICT (user_id, service_id) DO UPDATE SET
				encrypted  = EXCLUDED.encrypted,
				iv         = EXCLUDED.iv,
				auth_tag   = EXCLUDED.auth_tag,
				kind       = 'ref',
				updated_at = NOW()`
	} else {
		query = `
			INSERT INTO vault_entries (id, user_id, service_id, encrypted, iv, auth_tag, kind)
			VALUES (?, ?, ?, ?, ?, ?, 'ref')
			ON CONFLICT (user_id, service_id) DO UPDATE SET
				encrypted  = excluded.encrypted,
				iv         = excluded.iv,
				auth_tag   = excluded.auth_tag,
				kind       = 'ref',
				updated_at = CURRENT_TIMESTAMP`
	}
	_, err = s.crypto.db.ExecContext(ctx, query, id, userID, serviceID, encrypted, iv, authTag)
	return err
}

// resetKindToPush marks any existing DB row for the pair 'push'. Called after
// a push write so a value that lands on a row previously holding a reference
// is never later reinterpreted as an envelope. Under `gcp` push there is
// normally no DB row for a push entry, so this is a harmless no-op there.
func (s refKindStore) resetKindToPush(ctx context.Context, userID, serviceID string) error {
	q := fmt.Sprintf(
		`UPDATE vault_entries SET kind = 'push' WHERE user_id = %s AND service_id = %s AND kind <> 'push'`,
		s.ph(1), s.ph(2),
	)
	_, err := s.crypto.db.ExecContext(ctx, q, userID, serviceID)
	return err
}

// deleteRow removes the DB ref row for the pair (used by Delete to clean up
// ref rows under the `gcp` push backend, where inner.Delete only touches
// Secret Manager).
func (s refKindStore) deleteRow(ctx context.Context, userID, serviceID string) error {
	q := fmt.Sprintf(
		`DELETE FROM vault_entries WHERE user_id = %s AND service_id = %s AND kind = 'ref'`,
		s.ph(1), s.ph(2),
	)
	_, err := s.crypto.db.ExecContext(ctx, q, userID, serviceID)
	return err
}

// listRefServiceIDs returns serviceIDs of ref rows for userID.
func (s refKindStore) listRefServiceIDs(ctx context.Context, userID string) ([]string, error) {
	q := fmt.Sprintf(
		`SELECT service_id FROM vault_entries WHERE user_id = %s AND kind = 'ref' ORDER BY service_id`,
		s.ph(1),
	)
	rows, err := s.crypto.db.QueryContext(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("vault list refs: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var svc string
		if err := rows.Scan(&svc); err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

// ReferenceVault wraps the configured push backend and adds per-entry
// external-secret references. It implements the Vault interface; SetReference
// is an additional concrete method (deliberately NOT on the interface, so every
// existing Vault implementer keeps compiling — see the spec's guardrails).
type ReferenceVault struct {
	inner     Vault
	store     refKindStore
	resolvers map[string]Resolver
	allowlist []string
}

var _ Vault = (*ReferenceVault)(nil)

// NewReferenceVault builds a ReferenceVault around a push backend. crypto is a
// LocalVault (master-key + DB) used solely to seal/open ref envelopes and to
// own the kind column; it may be the same *LocalVault as inner under the
// `local` backend. resolvers maps backend id -> Resolver (lazy clients).
// allowlist is the set of permitted reference-id prefixes; an empty allowlist
// disables reference creation entirely (fail closed).
func NewReferenceVault(inner Vault, crypto *LocalVault, resolvers map[string]Resolver, allowlist []string) *ReferenceVault {
	return &ReferenceVault{
		inner:     inner,
		store:     refKindStore{crypto: crypto},
		resolvers: resolvers,
		allowlist: allowlist,
	}
}

// Set stores a pushed credential (kind='push'), delegating to the push
// backend. It then ensures the row's kind is 'push' so a value overwriting a
// previous reference is never reinterpreted as an envelope.
func (v *ReferenceVault) Set(ctx context.Context, userID, serviceID string, credential []byte) error {
	if err := v.inner.Set(ctx, userID, serviceID, credential); err != nil {
		return err
	}
	return v.store.resetKindToPush(ctx, userID, serviceID)
}

// SetIfAbsent mirrors Set for the create-only path.
func (v *ReferenceVault) SetIfAbsent(ctx context.Context, userID, serviceID string, credential []byte) error {
	if err := v.inner.SetIfAbsent(ctx, userID, serviceID, credential); err != nil {
		return err
	}
	return v.store.resetKindToPush(ctx, userID, serviceID)
}

// Get resolves a reference to live plaintext at read time, or delegates to
// the push backend for a pushed value. The resolved plaintext is NEVER
// written anywhere (no caching in v1).
func (v *ReferenceVault) Get(ctx context.Context, userID, serviceID string) ([]byte, error) {
	kind, exists, err := v.store.readKind(ctx, userID, serviceID)
	if err != nil {
		return nil, err
	}
	if exists && kind == KindRef {
		env, err := v.store.getEnvelope(ctx, userID, serviceID)
		if err != nil {
			return nil, err
		}
		return v.resolve(ctx, env)
	}
	return v.inner.Get(ctx, userID, serviceID)
}

// Delete removes both a pushed value (via the push backend) and any DB ref
// row for the pair.
func (v *ReferenceVault) Delete(ctx context.Context, userID, serviceID string) error {
	if err := v.inner.Delete(ctx, userID, serviceID); err != nil {
		return err
	}
	return v.store.deleteRow(ctx, userID, serviceID)
}

// List returns serviceIDs of BOTH kinds. Under `local` the push backend's
// List already includes ref rows (same table); under `gcp` it does not, so we
// union in the DB ref rows. The result is deduplicated and sorted.
func (v *ReferenceVault) List(ctx context.Context, userID string) ([]string, error) {
	pushed, err := v.inner.List(ctx, userID)
	if err != nil {
		return nil, err
	}
	refs, err := v.store.listRefServiceIDs(ctx, userID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(pushed)+len(refs))
	out := make([]string, 0, len(pushed)+len(refs))
	for _, s := range pushed {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, s := range refs {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// SetReference validates the target against the allowlist and stores an
// encrypted ref envelope (kind='ref'). It does NOT contact the backend —
// callers wanting a fail-fast dry run call Verify separately (?verify=1).
func (v *ReferenceVault) SetReference(ctx context.Context, userID, serviceID string, env RefEnvelope) error {
	if err := v.checkAllowed(env.ID); err != nil {
		return err
	}
	if _, ok := v.resolvers[env.Backend]; !ok {
		return fmt.Errorf("%w: %q", ErrRefBackendUnknown, env.Backend)
	}
	return v.store.putEnvelope(ctx, userID, serviceID, env)
}

// Verify performs a dry-run resolve of a candidate envelope and discards the
// plaintext. Used by the ?verify=1 create path and the wizard to fail fast on
// a bad reference. It also enforces the allowlist so verify cannot probe
// arbitrary instance-readable secrets.
func (v *ReferenceVault) Verify(ctx context.Context, env RefEnvelope) error {
	if err := v.checkAllowed(env.ID); err != nil {
		return err
	}
	_, err := v.resolve(ctx, env)
	return err
}

// EntryKind reports the stored kind for (userID, serviceID): KindPush,
// KindRef, or "" when no DB row exists (e.g. a push entry under the gcp
// backend). Used by handlers/UI to distinguish without decrypting.
func (v *ReferenceVault) EntryKind(ctx context.Context, userID, serviceID string) (string, error) {
	kind, exists, err := v.store.readKind(ctx, userID, serviceID)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", nil
	}
	return kind, nil
}

// ListReferenceServiceIDs returns the serviceIDs stored as references for
// userID. Handlers/UI use it to tag List() results without decrypting.
func (v *ReferenceVault) ListReferenceServiceIDs(ctx context.Context, userID string) ([]string, error) {
	return v.store.listRefServiceIDs(ctx, userID)
}

func (v *ReferenceVault) resolve(ctx context.Context, env RefEnvelope) ([]byte, error) {
	resolver, ok := v.resolvers[env.Backend]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrRefBackendUnknown, env.Backend)
	}
	return resolveWithRetry(ctx, resolver, env)
}

// checkAllowed enforces the operator's reference-id prefix allowlist. An empty
// allowlist means references are disabled (fail closed). The returned error is
// generic and never echoes the allowlist.
func (v *ReferenceVault) checkAllowed(id string) error {
	for _, prefix := range v.allowlist {
		if prefix != "" && strings.HasPrefix(id, prefix) {
			return nil
		}
	}
	return ErrRefTargetNotAllowed
}

// resolveWithRetry retries only on ErrRefThrottled: 3 attempts total,
// exponential backoff with a 100ms base and jitter.
func resolveWithRetry(ctx context.Context, r Resolver, env RefEnvelope) ([]byte, error) {
	const maxAttempts = 3
	const base = 100 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		out, err := r.Resolve(ctx, env)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !errors.Is(err, ErrRefThrottled) {
			return nil, err
		}
		if attempt == maxAttempts-1 {
			break
		}
		// Exponential backoff (base * 2^attempt) plus up to base jitter.
		delay := base * time.Duration(1<<attempt)
		delay += time.Duration(rand.Int63n(int64(base))) //nolint:gosec // jitter, not security-sensitive
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, lastErr
}

// ExtractJSONKey applies a RefEnvelope's json_key to a fetched secret payload:
// when json_key is empty the raw bytes are returned; otherwise the payload is
// parsed as a JSON object and the named field extracted. A missing key yields
// ErrRefKeyMissing WITHOUT naming the key or listing available keys. Shared by
// all resolvers so the structure-disclosure guarantee holds uniformly.
func ExtractJSONKey(payload []byte, jsonKey string) ([]byte, error) {
	if jsonKey == "" {
		return payload, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		// The payload isn't a JSON object but a key was requested.
		return nil, ErrRefKeyMissing
	}
	raw, ok := obj[jsonKey]
	if !ok {
		return nil, ErrRefKeyMissing
	}
	// A JSON string value is unquoted to its underlying bytes; any other
	// JSON type is returned as its raw encoding.
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return []byte(asString), nil
	}
	return raw, nil
}
