#!/usr/bin/env bash
#
# tests/test_release_gates.sh - Comprehensive tests for release.sh gates and rollback
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
RELEASE_SH="${REPO_ROOT}/scripts/release.sh"

RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

pass() {
    echo -e "${GREEN}[TEST PASS]${NC} $*"
}

fail() {
    echo -e "${RED}[TEST FAIL]${NC} $*" >&2
    exit 1
}

echo "=== Running Release Gate Tests ==="

# Test 1: release.sh build produces candidate binary without touching prod
echo "--- Test 1: Build isolation ---"
rm -f "${REPO_ROOT}/bin/candidate/cliproxyapi"
"${RELEASE_SH}" build >/dev/null 2>&1
[[ -x "${REPO_ROOT}/bin/candidate/cliproxyapi" ]] || fail "Candidate binary not built"
pass "Test 1 passed: Candidate built in isolation"

# Test 2: release.sh verify positive test
echo "--- Test 2: Verify positive control ---"
"${RELEASE_SH}" verify >/dev/null 2>&1 || fail "Verify positive run failed"
pass "Test 2 passed: Real startup verification succeeded on isolated port"

# Test 3: release.sh verify negative test (broken binary)
echo "--- Test 3: Verify negative control (broken binary interception) ---"
BROKEN_BIN="${REPO_ROOT}/bin/candidate/cliproxyapi"
mv "${BROKEN_BIN}" "${BROKEN_BIN}.real"
cat > "${BROKEN_BIN}" << 'EOF'
#!/bin/sh
echo "corrupted binary dummy" >&2
exit 1
EOF
chmod +x "${BROKEN_BIN}"

if "${RELEASE_SH}" verify >/dev/null 2>&1; then
    mv "${BROKEN_BIN}.real" "${BROKEN_BIN}"
    fail "Verify should have rejected broken binary but passed"
fi
mv "${BROKEN_BIN}.real" "${BROKEN_BIN}"
pass "Test 3 passed: Broken binary correctly intercepted before deployment"

# Test 4: Automatic rollback test in simulated production sandbox
echo "--- Test 4: Simulated failure & automatic rollback ---"
SIM_DIR="$(mktemp -d /tmp/sim-cliproxy.XXXXXX)"
SIM_TARGET="${SIM_DIR}/bin/cliproxyapi"
SIM_CONFIG="${SIM_DIR}/cliproxyapi.conf"
mkdir -p "${SIM_DIR}/bin"

# Create simulated original production binary
cat > "${SIM_TARGET}" << 'EOF'
#!/bin/sh
echo "ORIGINAL_PROD_V1"
EOF
chmod 755 "${SIM_TARGET}"

cat > "${SIM_CONFIG}" << 'EOF'
port: 19999
api-keys:
  - "sim-key"
EOF

# Run deploy with simulated target and failing restart/healthcheck
export TARGET_BIN="${SIM_TARGET}"
export PROD_CONFIG="${SIM_CONFIG}"
export RESTART_CMD="true" # simulated restart succeeds, but healthcheck on port 19999 will fail

set +e
"${RELEASE_SH}" deploy > "${SIM_DIR}/deploy.log" 2>&1
DEPLOY_CODE=$?
set -e

if [[ "${DEPLOY_CODE}" -eq 0 ]]; then
    fail "Deploy should have failed and returned non-zero code"
fi

# Verify rollback restored the original content
RESTORED_CONTENT="$(cat "${SIM_TARGET}")"
if [[ "${RESTORED_CONTENT}" != *"ORIGINAL_PROD_V1"* ]]; then
    fail "Rollback did not restore original binary! Content: ${RESTORED_CONTENT}"
fi

# Verify backup was created
if [[ ! -f "${SIM_DIR}/bin/cliproxyapi.bak-latest" ]]; then
    fail "cliproxyapi.bak-latest not created"
fi

rm -rf "${SIM_DIR}"
pass "Test 4 passed: Deploy failed healthcheck, automatic rollback restored original binary"

echo ""
echo -e "${GREEN}All release gate test cases passed successfully!${NC}"
