#!/usr/bin/env bash
# check-test-names.sh - Enforce Google Go Style Guide test naming conventions
#
# This script checks that t.Run() subtest names use spaces, not underscores.
# Google Go Style Guide recommends descriptive names like "returns empty list when no events"
# instead of "returns_empty_list_when_no_events".
#
# Test function names MAY include underscores (Google Go Style Guide):
#   Good: func TestEventLog_Append(t *testing.T)
#   Good: func TestConfig_Load(t *testing.T)
#   Good: func TestIntegration_KernelStartStop(t *testing.T)
#
# This follows the actual practice in Google's googleapis/google-cloud-go repository.
#
# Usage:
#   ./scripts/check-test-names.sh                    # Check all test files
#   ./scripts/check-test-names.sh internal/kernel/   # Check specific directory

set -euo pipefail

# ANSI color codes
RED='\033[0;31m'
YELLOW='\033[1;33m'
GREEN='\033[0;32m'
NC='\033[0m' # No Color

# Default to all test files if no arguments provided
SEARCH_PATH="${1:-.}"

violations_found=false

# Check: t.Run() subtest names should use spaces, not underscores
# Pattern explanation:
# - t\.Run\(" - matches t.Run("
# - [^"]*_ - matches any characters except quotes, followed by underscore
# - Excludes field names like create_time, update_time, entity_id (underscore with space before/after)
subtest_violations=$(grep -rn 't\.Run("[^"]*_[^"]*"' "$SEARCH_PATH" --include="*_test.go" \
  | grep -v '_time[" ]' \
  | grep -v '_id[" ]' \
  | grep -v '_by[" ]' \
  | grep -v ' _' \
  | grep -v '_ ' || true)

if [ -n "$subtest_violations" ]; then
  violations_found=true
  echo -e "${RED}Subtest naming violations found${NC}"
  echo ""
  echo "t.Run() subtest names should use spaces, not underscores."
  echo "Example: t.Run(\"returns empty list\") not t.Run(\"returns_empty_list\")"
  echo ""
  echo -e "${YELLOW}Violations:${NC}"
  echo "$subtest_violations"
  echo ""
fi

if [ "$violations_found" = true ]; then
  exit 1
else
  echo -e "${GREEN}All test names follow Google Go Style Guide conventions${NC}"
  echo "  - t.Run() names use spaces (not underscores)"
  echo "  - Test function names may use underscores for grouping (TestType_Method)"
  exit 0
fi
