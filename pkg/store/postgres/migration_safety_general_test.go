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

var whitespaceRun = regexp.MustCompile(`\s+`)

// normalizeSQL strips -- comments and collapses whitespace so multi-line
// statements match the same patterns as single-line ones. Shared by both
// migration guards so they agree on what counts as executable SQL — a checker
// that reads comments differently from its sibling produces findings that
// disagree on the same file.
//
// Comment stripping is literal-aware: a value like 'US -- New York' must not
// truncate the statement, because a truncated statement can drop the very
// keyword being searched for and turn a violation into a silent pass. Quote
// state resets per line, which is deliberate — an unbalanced quote leaves the
// rest of that line looking quoted, so a trailing comment is retained rather
// than dropped. That errs toward a false positive (a failing guard someone
// investigates) instead of a false negative (a migration that slips through).
func normalizeSQL(raw string) string {
	var active []string
	for _, line := range strings.Split(raw, "\n") {
		active = append(active, stripLineComment(line))
	}
	return whitespaceRun.ReplaceAllString(strings.Join(active, " "), " ")
}

// stripLineComment truncates line at the first -- that is not inside a
// single-quoted literal. An escaped quote inside a literal is written as two
// consecutive apostrophes, which toggle the flag twice and so need no special
// handling.
func stripLineComment(line string) string {
	inQuote := false
	for i := 0; i < len(line)-1; i++ {
		switch {
		case line[i] == '\'':
			inQuote = !inQuote
		case !inQuote && line[i] == '-' && line[i+1] == '-':
			return line[:i]
		}
	}
	return line
}

// largeTableCheck is one dangerous statement shape, plus the escape hatch that
// makes that shape acceptable.
type largeTableCheck struct {
	pattern *regexp.Regexp
	// okIfContains exempts a match whose statement text contains this
	// substring, expressing "this shape is fine as long as it is qualified".
	okIfContains string
	why          string
}

// largeTableChecks builds the guard patterns. Extracted from the scanning test
// so TestGuardCatchesEvasions can exercise the same patterns the real scan
// uses — a guard verified against a different regex than it ships is not
// verified at all.
func largeTableChecks() []largeTableCheck {
	// tableRef spells the ways a migration can name one of the large tables:
	// bare, schema-qualified, ONLY-prefixed, or double-quoted. Every check
	// composes this one fragment rather than spelling it out, because when the
	// patterns each carried their own copy they drifted — the UPDATE check
	// lacked the public. prefix the others had, so UPDATE public.audit_log
	// passed the guard clean.
	tableRef := `(?:ONLY\s+)?(?:"?public"?\.)?"?(?:` + strings.Join(largeTables, "|") + `)"?`

	return []largeTableCheck{
		{
			pattern: regexp.MustCompile(`(?i)\bUPDATE\s+` + tableRef + `\b`),
			why: "rewrites every row of a large table inside the startup transaction; " +
				"defer it to a batched script like scripts/postgres/post-062-backfill.sql",
		},
		{
			pattern:      regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+[^;]*?\bON\s+` + tableRef + `\b[^;]*;`),
			okIfContains: "CONCURRENTLY",
			why: "builds an index over a large table; CREATE INDEX cannot run CONCURRENTLY " +
				"inside the transactional runner, so move it to the post-migration script",
		},
		{
			pattern:      regexp.MustCompile(`(?i)ALTER\s+TABLE\s+` + tableRef + `\s+ADD\s+CONSTRAINT\b[^;]*;`),
			okIfContains: "NOT VALID",
			why: "adds a constraint to a large table without NOT VALID, forcing a full " +
				"validation scan while holding the DDL lock; add NOT VALID and " +
				"VALIDATE CONSTRAINT separately",
		},
	}
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
	checks := largeTableChecks()

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

// TestGuardCatchesEvasions pins the spellings a dangerous statement can take.
// The guard's whole value is catching a migration nobody reviewed carefully,
// so a gap here is silent: the scan passes and the migration ships. Each
// wantCaught case below is a real evasion that slipped through at some point
// or plausibly could.
func TestGuardCatchesEvasions(t *testing.T) {
	checks := largeTableChecks()
	caught := func(sql string) bool {
		norm := normalizeSQL(sql)
		for _, c := range checks {
			for _, m := range c.pattern.FindAllString(norm, -1) {
				if c.okIfContains != "" && strings.Contains(strings.ToUpper(m), c.okIfContains) {
					continue
				}
				return true
			}
		}
		return false
	}

	wantCaught := map[string]string{
		"bare":                 `UPDATE audit_log SET actor_email = '';`,
		"schema qualified":     `UPDATE public.audit_log SET actor_email = '';`,
		"ONLY prefixed":        `UPDATE ONLY audit_log SET actor_email = '';`,
		"quoted identifier":    `UPDATE "audit_log" SET actor_email = '';`,
		"lowercase keywords":   `update audit_log set actor_email = '';`,
		"mixed case":           `UpDaTe Public.Audit_Log SET actor_email = '';`,
		"newline before table": "UPDATE\n    audit_log\n    SET actor_email = '';",
		"other large table":    `UPDATE gateway_request_log SET reason = '';`,
		"non-concurrent index": `CREATE INDEX idx_x ON audit_log(timestamp DESC);`,
		"index schema qual":    `CREATE INDEX idx_x ON public.audit_log(timestamp);`,
		"fk without NOT VALID": `ALTER TABLE audit_log ADD CONSTRAINT fk FOREIGN KEY (user_id) REFERENCES users(id);`,
		// A -- inside a literal must not truncate the statement and hide the
		// UPDATE that follows it on the same line.
		"comment inside literal": `INSERT INTO t VALUES ('US -- NY'); UPDATE audit_log SET actor_email = '';`,
	}
	for name, sql := range wantCaught {
		t.Run("caught/"+name, func(t *testing.T) {
			if !caught(sql) {
				t.Errorf("guard missed a dangerous statement:\n  %s", sql)
			}
		})
	}

	wantAllowed := map[string]string{
		"concurrent index":     `CREATE INDEX CONCURRENTLY idx_x ON audit_log(timestamp DESC);`,
		"fk with NOT VALID":    `ALTER TABLE audit_log ADD CONSTRAINT fk FOREIGN KEY (user_id) REFERENCES users(id) NOT VALID;`,
		"small table update":   `UPDATE users SET verified_at = created_at WHERE verified_at IS NULL;`,
		"small table index":    `CREATE INDEX idx_c ON llm_request_cost(timestamp);`,
		"add column":           `ALTER TABLE audit_log ADD COLUMN actor_email TEXT NOT NULL DEFAULT '';`,
		"validate constraint":  `ALTER TABLE audit_log VALIDATE CONSTRAINT audit_log_user_id_fkey;`,
		"commented-out update": `-- UPDATE audit_log SET actor_email = '';`,
		"trailing comment":     `SELECT 1; -- UPDATE audit_log is deferred to the backfill script`,
	}
	for name, sql := range wantAllowed {
		t.Run("allowed/"+name, func(t *testing.T) {
			if caught(sql) {
				t.Errorf("guard falsely flagged a safe statement:\n  %s", sql)
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
