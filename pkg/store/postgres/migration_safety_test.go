package postgres

import (
	"strings"
	"testing"
)

// TestLargeTableMigrationsStayStartupSafe guards against putting unbounded
// production data work back into the transactional startup runner. These
// statements caused Cloud Run startup timeouts and held audit_log unavailable
// while millions of rows were rewritten.
func TestLargeTableMigrationsStayStartupSafe(t *testing.T) {
	tests := []struct {
		name       string
		migration  string
		required   []string
		prohibited []string
	}{
		{
			name:      "actor retention defers historical backfill",
			migration: "migrations/062_audit_actor_retain.sql",
			required: []string{
				"SET LOCAL lock_timeout",
				"ON DELETE SET NULL NOT VALID",
			},
			prohibited: []string{
				"UPDATE audit_log",
				"UPDATE llm_request_cost",
			},
		},
		{
			name:      "large audit index is not built at startup",
			migration: "migrations/064_admin_visibility_indexes.sql",
			prohibited: []string{
				"CREATE INDEX IF NOT EXISTS idx_audit_time",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := migrationsFS.ReadFile(tt.migration)
			if err != nil {
				t.Fatalf("read %s: %v", tt.migration, err)
			}
			// Operator instructions in comments intentionally show the deferred
			// statements, so only executable SQL is inspected. Uses the same
			// normalizeSQL as the general guard — when this test stripped
			// comments its own way it disagreed with its sibling about which
			// bytes were executable, including on inline trailing comments.
			//
			// Compared case-insensitively: SQL keywords are, so a migration
			// written as `update audit_log` would otherwise sail past a
			// prohibition spelled `UPDATE audit_log`. Needles below are
			// uppercase-normalized too, so their casing here is cosmetic.
			sql := strings.ToUpper(normalizeSQL(string(data)))
			for _, required := range tt.required {
				if !strings.Contains(sql, strings.ToUpper(required)) {
					t.Errorf("%s must contain %q", tt.migration, required)
				}
			}
			for _, prohibited := range tt.prohibited {
				if strings.Contains(sql, strings.ToUpper(prohibited)) {
					t.Errorf("%s must not contain %q", tt.migration, prohibited)
				}
			}
		})
	}
}
