package postgres

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// largeTables are append-only tables whose row count grows with request
// traffic rather than with the number of users, so on any long-running
// deployment they can reach millions of rows and multiple gigabytes. Any
// startup migration that rewrites one of them, or builds an index over one
// non-concurrently, holds a lock for the duration and takes the deployment
// down with it.
var largeTables = []string{"audit_log", "gateway_request_log"}

// grandfatheredMigrations already shipped and are recorded in production's
// schema_migrations. Rewriting them would not un-run them, and renaming a
// file makes the runner re-apply it, so they are pinned here instead. Do NOT
// add to this list to silence a failure on a new migration — fix the
// migration. See scripts/postgres/post-062-backfill.sql for the pattern to
// follow when large-table data work is genuinely required.
var grandfatheredMigrations = map[string]string{
	"003_audit.sql":                            "creates audit_log and its indexes; table is empty at creation",
	"026_request_log.sql":                      "creates gateway_request_log and its indexes; table is empty at creation",
	"028_audit_request_id_user_unique.sql":     "shipped before audit_log grew large",
	"029_indexes.sql":                          "shipped before the tables grew large",
	"038_audit_runtime_expression_indexes.sql": "shipped before audit_log grew large",
	"044_symmetric_dedup_scope.sql":            "shipped before audit_log grew large",
	"053_audit_dedup_key.sql":                  "shipped before audit_log grew large",
}

// normalizeSQL strips comments and collapses whitespace so multi-line
// statements match the same patterns as single-line ones.
func normalizeSQL(raw string) string {
	var active []string
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		active = append(active, line)
	}
	return regexp.MustCompile(`\s+`).ReplaceAllString(strings.Join(active, " "), " ")
}

// TestNoUnboundedLargeTableWorkInStartupMigrations scans every migration --
// not just the two that caused the incident -- so a future migration cannot
// reintroduce the same failure mode. The startup runner wraps each file in a
// single transaction and the server exits non-zero if it fails, so anything
// slow here is downtime, not degradation.
func TestNoUnboundedLargeTableWorkInStartupMigrations(t *testing.T) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}

	tableAlt := strings.Join(largeTables, "|")
	checks := []struct {
		pattern *regexp.Regexp
		// okIfContains exempts a match whose statement text contains this
		// substring, expressing "this shape is fine as long as it is qualified".
		okIfContains string
		why          string
	}{
		{
			pattern: regexp.MustCompile(`(?i)\bUPDATE\s+(` + tableAlt + `)\b`),
			why: "rewrites every row of a large table inside the startup transaction; " +
				"defer it to a batched script like scripts/postgres/post-062-backfill.sql",
		},
		{
			pattern:      regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+[^;]*?\bON\s+(?:public\.)?(?:` + tableAlt + `)\b[^;]*;`),
			okIfContains: "CONCURRENTLY",
			why: "builds an index over a large table; CREATE INDEX cannot run CONCURRENTLY " +
				"inside the transactional runner, so move it to the post-migration script",
		},
		{
			pattern:      regexp.MustCompile(`(?i)ALTER\s+TABLE\s+(?:public\.)?(?:` + tableAlt + `)\s+ADD\s+CONSTRAINT\b[^;]*;`),
			okIfContains: "NOT VALID",
			why: "adds a constraint to a large table without NOT VALID, forcing a full " +
				"validation scan while holding the DDL lock; add NOT VALID and " +
				"VALIDATE CONSTRAINT separately",
		},
	}

	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			if why, ok := grandfatheredMigrations[name]; ok {
				t.Skipf("grandfathered: %s", why)
			}
			data, err := migrationsFS.ReadFile("migrations/" + name)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			sql := normalizeSQL(string(data))
			for _, c := range checks {
				for _, m := range c.pattern.FindAllString(sql, -1) {
					if c.okIfContains != "" && strings.Contains(strings.ToUpper(m), c.okIfContains) {
						continue
					}
					t.Errorf("%s is not startup-safe.\n  statement: %s\n  reason: %s",
						name, strings.TrimSpace(m), c.why)
				}
			}
		})
	}
}

// TestGrandfatherListIsAccurate keeps the allowlist honest: an entry that no
// longer corresponds to a real file is stale and should be removed, so the
// list cannot quietly accumulate dead exemptions.
func TestGrandfatherListIsAccurate(t *testing.T) {
	for name := range grandfatheredMigrations {
		if _, err := migrationsFS.ReadFile("migrations/" + name); err != nil {
			t.Errorf("grandfathered migration %q no longer exists: %v", name, err)
		}
	}
}
