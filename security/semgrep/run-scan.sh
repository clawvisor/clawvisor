#!/usr/bin/env bash
set -euo pipefail

# Run Semgrep SAST scan against Clawvisor
#
# Usage:
#   ./security/semgrep/run-scan.sh          # Full scan (Go + TypeScript)
#   ./security/semgrep/run-scan.sh go       # Go backend only
#   ./security/semgrep/run-scan.sh web      # TypeScript frontend only

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
REPORT_DIR="$SCRIPT_DIR/reports"

if ! command -v semgrep &>/dev/null; then
  echo "Error: semgrep not found"
  echo "Install with: pip3 install semgrep"
  exit 1
fi

mkdir -p "$REPORT_DIR"

TARGET="${1:-all}"

# Rulesets: OWASP top 10, language-specific security, default recommendations
RULESETS="p/owasp-top-ten p/security-audit p/secrets"

echo "=== Semgrep SAST Scan ==="
echo "Version: $(semgrep --version)"
echo "Target:  $TARGET"
echo ""

run_go_scan() {
  echo "--- Scanning Go backend ---"
  semgrep scan \
    --config "p/owasp-top-ten" \
    --config "p/golang" \
    --config "p/security-audit" \
    --config "p/secrets" \
    --exclude "vendor" \
    --exclude "web" \
    --exclude "docs" \
    --exclude "security" \
    --exclude "e2e" \
    --json \
    --output "$REPORT_DIR/go-results.json" \
    "$PROJECT_DIR" 2>&1 | tee "$REPORT_DIR/go-scan.log"

  echo ""
  echo "--- Go scan summary ---"
  semgrep scan \
    --config "p/owasp-top-ten" \
    --config "p/golang" \
    --config "p/security-audit" \
    --config "p/secrets" \
    --exclude "vendor" \
    --exclude "web" \
    --exclude "docs" \
    --exclude "security" \
    --exclude "e2e" \
    --text \
    "$PROJECT_DIR" 2>&1 | tee "$REPORT_DIR/go-results.txt"
}

run_web_scan() {
  echo "--- Scanning TypeScript frontend ---"
  semgrep scan \
    --config "p/owasp-top-ten" \
    --config "p/typescript" \
    --config "p/react" \
    --config "p/security-audit" \
    --config "p/secrets" \
    --exclude "node_modules" \
    --exclude "dist" \
    --json \
    --output "$REPORT_DIR/web-results.json" \
    "$PROJECT_DIR/web" 2>&1 | tee "$REPORT_DIR/web-scan.log"

  echo ""
  echo "--- Web scan summary ---"
  semgrep scan \
    --config "p/owasp-top-ten" \
    --config "p/typescript" \
    --config "p/react" \
    --config "p/security-audit" \
    --config "p/secrets" \
    --exclude "node_modules" \
    --exclude "dist" \
    --text \
    "$PROJECT_DIR/web" 2>&1 | tee "$REPORT_DIR/web-results.txt"
}

case "$TARGET" in
  go)   run_go_scan ;;
  web)  run_web_scan ;;
  all)  run_go_scan; echo ""; run_web_scan ;;
  *)    echo "Usage: $0 [all|go|web]"; exit 1 ;;
esac

echo ""
echo "=== Scan complete ==="
echo "Reports: $REPORT_DIR/"
ls -la "$REPORT_DIR"/*.json "$REPORT_DIR"/*.txt 2>/dev/null || echo "(no reports generated)"
