#!/usr/bin/env bash
#
# scripts/release.sh - CLIProxyAPI Standard Build, Verify, and Release Pipeline
#
# Enforces release gates:
# 1. Isolation: Builds to candidate dir (bin/candidate/cliproxyapi), never directly overwrites running binary.
# 2. Unified Entrypoint: Injects Version, Commit, BuildDate, DefaultConfigPath centrally.
# 3. Real Startup Verification: Runs candidate on an isolated port & temp data dir with real flags,
#    verifying config parsing, HTTP endpoints, and positive/negative auth checks (--help is not enough).
# 4. Atomic Replacement & Automatic Rollback: Backs up current binary, atomically swaps via mv,
#    restarts service, and automatically restores previous backup if healthcheck fails.
# 5. Independent Execution: Self-contained script that completes verification, restart, and rollback
#    without requiring intermediate external commands.
#

set -euo pipefail

# --- Color formatting ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${BLUE}[INFO]${NC} $*"
}

log_ok() {
    echo -e "${GREEN}[OK]${NC} $*"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $*"
}

log_err() {
    echo -e "${RED}[ERROR]${NC} $*" >&2
}

log_rollback() {
    echo -e "${RED}[ROLLBACK]${NC} $*" >&2
}

# --- Paths & Configuration ---
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

CANDIDATE_DIR="${REPO_ROOT}/bin/candidate"
CANDIDATE_BIN="${CANDIDATE_DIR}/cliproxyapi"

TARGET_BIN="${TARGET_BIN:-/opt/homebrew/opt/cliproxyapi/bin/cliproxyapi}"
TARGET_DIR="$(dirname "${TARGET_BIN}")"
PROD_CONFIG="${PROD_CONFIG:-/opt/homebrew/etc/cliproxyapi.conf}"
DEFAULT_CONFIG_PATH="/opt/homebrew/etc/cliproxyapi.conf"

LATEST_BACKUP="${TARGET_DIR}/cliproxyapi.bak-latest"

# Metadata calculation
COMMIT="$(git -C "${REPO_ROOT}" rev-parse --short HEAD 2>/dev/null || echo "none")"
VERSION="${VERSION:-7.3.5-local}"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# --- Helper: Find Free High Port ---
find_free_port() {
    local default_port="${1:-18317}"
    python3 -c "
import socket
s = socket.socket()
try:
    s.bind(('127.0.0.1', ${default_port}))
    port = ${default_port}
except OSError:
    s.bind(('127.0.0.1', 0))
    port = s.getsockname()[1]
s.close()
print(port)
"
}

# --- Stage 1: Build Candidate Binary ---
build_candidate() {
    log_info "=== Stage 1: Building Candidate Binary ==="
    log_info "Repository:        ${REPO_ROOT}"
    log_info "Version:           ${VERSION}"
    log_info "Commit:            ${COMMIT}"
    log_info "Build Date:        ${BUILD_DATE}"
    log_info "DefaultConfigPath: ${DEFAULT_CONFIG_PATH}"
    log_info "Output:            ${CANDIDATE_BIN}"

    mkdir -p "${CANDIDATE_DIR}"

    local ldflags="-X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildDate=${BUILD_DATE} -X main.DefaultConfigPath=${DEFAULT_CONFIG_PATH} -s -w"

    (
        cd "${REPO_ROOT}"
        go build -ldflags "${ldflags}" -o "${CANDIDATE_BIN}" ./cmd/server
    )

    if [[ ! -x "${CANDIDATE_BIN}" ]]; then
        log_err "Build failed: ${CANDIDATE_BIN} not found or not executable"
        exit 1
    fi

    local size
    size="$(ls -lh "${CANDIDATE_BIN}" | awk '{print $5}')"
    log_ok "Candidate built successfully (${size}) -> ${CANDIDATE_BIN}"
}

# --- Stage 2: Real Startup Verification on Isolated Port ---
verify_candidate() {
    log_info "=== Stage 2: Verifying Candidate on Isolated Port ==="

    if [[ ! -x "${CANDIDATE_BIN}" ]]; then
        log_err "Candidate binary not found at ${CANDIDATE_BIN}. Run build first."
        exit 1
    fi

    local test_port
    test_port="$(find_free_port 18317)"
    local test_dir
    test_dir="$(mktemp -d /tmp/cliproxy-verify.XXXXXX)"
    local test_config="${test_dir}/cliproxyapi.conf"
    local test_auth_dir="${test_dir}/auths"
    local test_log="${test_dir}/server.log"
    local test_key="cpa-test-verification-key-12345"
    local test_secret="test-management-secret"

    mkdir -p "${test_auth_dir}"

    cat > "${test_config}" <<EOF
host: "127.0.0.1"
port: ${test_port}
auth-dir: "${test_auth_dir}"
api-keys:
  - "${test_key}"
remote-management:
  allow-remote: false
  secret-key: "${test_secret}"
  disable-control-panel: true
  disable-auto-update-panel: true
debug: true
EOF

    log_info "Starting candidate process on test port ${test_port} with config ${test_config}..."
    "${CANDIDATE_BIN}" --config "${test_config}" > "${test_log}" 2>&1 &
    local test_pid=$!

    cleanup_verify() {
        if kill -0 "${test_pid}" 2>/dev/null; then
            kill -TERM "${test_pid}" 2>/dev/null || kill -9 "${test_pid}" 2>/dev/null || true
            wait "${test_pid}" 2>/dev/null || true
        fi
        rm -rf "${test_dir}"
    }
    trap cleanup_verify EXIT

    # Poll for service readiness (up to 10 seconds)
    local ready=0
    for ((i=1; i<=50; i++)); do
        if ! kill -0 "${test_pid}" 2>/dev/null; then
            log_err "Candidate process died prematurely. Server log:"
            tail -n 30 "${test_log}" >&2 || true
            exit 1
        fi

        if curl -s -f "http://127.0.0.1:${test_port}/" >/dev/null 2>&1; then
            ready=1
            break
        fi
        sleep 0.2
    done

    if [[ ${ready} -ne 1 ]]; then
        log_err "Candidate failed to respond on http://127.0.0.1:${test_port}/ within 10s. Server log:"
        tail -n 30 "${test_log}" >&2 || true
        exit 1
    fi
    log_ok "Candidate successfully started and listening on 127.0.0.1:${test_port}"

    # Verify root endpoint & commit header using GET (Gin returns 404 on HEAD requests)
    local root_code
    root_code="$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:${test_port}/")"
    if [[ "${root_code}" != "200" ]]; then
        log_err "Root endpoint did not return 200 OK (got ${root_code})"
        exit 1
    fi

    local headers
    headers="$(curl -s -i "http://127.0.0.1:${test_port}/" | head -n 25)"
    if ! echo "${headers}" | grep -q "X-CPA-COMMIT"; then
        log_warn "X-CPA-COMMIT header not found in root response headers"
    fi

    # Positive control: valid API key returns 200
    local pos_code
    pos_code="$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer ${test_key}" "http://127.0.0.1:${test_port}/v1/models")"
    if [[ "${pos_code}" != "200" ]]; then
        log_err "Positive control failed: Expected HTTP 200 on /v1/models with valid key, got ${pos_code}"
        exit 1
    fi
    log_ok "Positive control passed: /v1/models returned 200 with valid API key"

    # Negative control 1: missing API key returns 401
    local neg_code_missing
    neg_code_missing="$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:${test_port}/v1/models")"
    if [[ "${neg_code_missing}" != "401" ]]; then
        log_err "Negative control 1 failed: Expected HTTP 401 on /v1/models without auth, got ${neg_code_missing}"
        exit 1
    fi
    log_ok "Negative control 1 passed: /v1/models returned 401 without auth"

    # Negative control 2: invalid API key returns 401
    local neg_code_invalid
    neg_code_invalid="$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer invalid-key-xyz" "http://127.0.0.1:${test_port}/v1/models")"
    if [[ "${neg_code_invalid}" != "401" ]]; then
        log_err "Negative control 2 failed: Expected HTTP 401 on /v1/models with bad key, got ${neg_code_invalid}"
        exit 1
    fi
    log_ok "Negative control 2 passed: /v1/models returned 401 with bad API key"

    # Clean shutdown of test process
    kill -TERM "${test_pid}" 2>/dev/null || true
    wait "${test_pid}" 2>/dev/null || true
    trap - EXIT
    rm -rf "${test_dir}"
    log_ok "Candidate passed all real startup and authentication tests"
}

# --- Service restart helper ---
restart_service() {
    if [[ -n "${RESTART_CMD:-}" ]]; then
        log_info "Executing custom restart command: ${RESTART_CMD}"
        eval "${RESTART_CMD}"
        return
    fi
    log_info "Restarting cliproxyapi service via launchctl / brew services..."
    local uid
    uid="$(id -u)"
    if launchctl print "gui/${uid}/sh.brew.cliproxyapi" >/dev/null 2>&1; then
        launchctl kickstart -k "gui/${uid}/sh.brew.cliproxyapi"
    else
        brew services restart cliproxyapi
    fi
}

# --- Production Health Check ---
check_prod_health() {
    local max_wait_sec="${1:-15}"
    local prod_port=8317
    local prod_key=""

    if [[ -f "${PROD_CONFIG}" ]]; then
        local extracted
        extracted="$(python3 -c '
import yaml, sys
try:
    with open(sys.argv[1], "r") as f:
        cfg = yaml.safe_load(f)
    port = cfg.get("port", 8317)
    keys = cfg.get("api-keys", [])
    key = keys[0] if keys else ""
    print(f"{port}|{key}")
except Exception:
    print("8317|")
' "${PROD_CONFIG}")"
        prod_port="${extracted%%|*}"
        prod_key="${extracted#*|}"
    fi

    log_info "Waiting for production service on 127.0.0.1:${prod_port} (timeout: ${max_wait_sec}s)..."
    local elapsed=0
    local healthy=0

    while (( elapsed < max_wait_sec * 2 )); do
        if curl -s -f "http://127.0.0.1:${prod_port}/" >/dev/null 2>&1; then
            healthy=1
            break
        fi
        sleep 0.5
        ((elapsed++))
    done

    if [[ ${healthy} -ne 1 ]]; then
        log_err "Production service failed to respond on 127.0.0.1:${prod_port} within ${max_wait_sec}s"
        return 1
    fi

    # Verify auth behavior if prod_key is available
    if [[ -n "${prod_key}" ]]; then
        local p_code
        p_code="$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer ${prod_key}" "http://127.0.0.1:${prod_port}/v1/models")"
        if [[ "${p_code}" != "200" ]]; then
            log_err "Production auth positive check failed: got HTTP ${p_code}"
            return 1
        fi

        local n_code
        n_code="$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:${prod_port}/v1/models")"
        if [[ "${n_code}" != "401" ]]; then
            log_err "Production auth negative check failed: got HTTP ${n_code}"
            return 1
        fi
    fi

    log_ok "Production service is healthy and authenticated on 127.0.0.1:${prod_port}"
    return 0
}

# --- Rollback Helper ---
rollback_production() {
    log_rollback "=== Triggering Automatic Rollback ==="
    if [[ ! -f "${LATEST_BACKUP}" ]]; then
        log_err "No backup found at ${LATEST_BACKUP}! Cannot automatically rollback."
        return 1
    fi

    log_warn "Restoring binary from ${LATEST_BACKUP} -> ${TARGET_BIN}"
    local tmp_restore="${TARGET_BIN}.rollback.$$"
    cp -p "${LATEST_BACKUP}" "${tmp_restore}"
    chmod 755 "${tmp_restore}"
    mv -f "${tmp_restore}" "${TARGET_BIN}"

    restart_service

    log_info "Verifying service recovery after rollback..."
    if check_prod_health 15; then
        log_ok "Service successfully restored to previous backup version."
    else
        log_err "CRITICAL: Service still unhealthy after rollback! Check logs: ~/Library/Logs/"
    fi
}

# --- Stage 3, 4, 5: Deploy with Atomic Swap & Auto-Rollback ---
deploy_candidate() {
    log_info "=== Stage 3: Atomic Replacement & Supervised Restart ==="

    if [[ ! -f "${TARGET_BIN}" ]]; then
        log_err "Target binary ${TARGET_BIN} does not exist"
        exit 1
    fi

    # Step 1: Backup current binary
    local ts
    ts="$(date +%Y%m%d%H%M%S)"
    local backup_file="${TARGET_DIR}/cliproxyapi.bak-pre-${COMMIT}-${ts}"
    log_info "Backing up current binary to ${backup_file} and ${LATEST_BACKUP}..."
    cp -p "${TARGET_BIN}" "${backup_file}"
    cp -p "${TARGET_BIN}" "${LATEST_BACKUP}"

    # Step 2: Atomic Replacement
    local tmp_target="${TARGET_BIN}.new.$$"
    log_info "Staging candidate binary to ${tmp_target}..."
    cp -p "${CANDIDATE_BIN}" "${tmp_target}"
    chmod 755 "${tmp_target}"

    local swap_done=0
    rollback_trap() {
        if [[ "${swap_done}" -eq 1 ]]; then
            log_rollback "Deploy failed or was interrupted after binary swap!"
            rollback_production || true
        fi
    }
    trap rollback_trap INT TERM ERR EXIT

    log_info "Atomically swapping ${tmp_target} -> ${TARGET_BIN}..."
    mv -f "${tmp_target}" "${TARGET_BIN}"
    swap_done=1

    # Step 3: Supervised Restart & Verification
    restart_service

    log_info "=== Stage 4: Verifying Production Health ==="
    if ! check_prod_health 15; then
        log_err "New binary failed production healthcheck!"
        rollback_production
        swap_done=0
        trap - INT TERM ERR EXIT
        exit 1
    fi

    # Release succeeded: disarm rollback trap
    swap_done=0
    trap - INT TERM ERR EXIT

    log_ok "=== Release Successfully Completed! ==="
    log_ok "Binary:  ${TARGET_BIN}"
    log_ok "Commit:  ${COMMIT}"
    log_ok "Backup:  ${backup_file}"
}

# --- Main Entrypoint ---
COMMAND="${1:-deploy}"

case "${COMMAND}" in
    build)
        build_candidate
        ;;
    verify)
        build_candidate
        verify_candidate
        ;;
    deploy)
        build_candidate
        verify_candidate
        deploy_candidate
        ;;
    rollback)
        rollback_production
        ;;
    help|-h|--help)
        echo "Usage: $0 [build|verify|deploy|rollback]"
        echo ""
        echo "Commands:"
        echo "  build     Build candidate binary to bin/candidate/cliproxyapi"
        echo "  verify    Build and run real startup verification on isolated port"
        echo "  deploy    Full pipeline: build -> verify -> backup -> atomic replace -> restart -> auto-rollback"
        echo "  rollback  Manually restore previous backup from cliproxyapi.bak-latest"
        echo ""
        echo "Environment variables:"
        echo "  VERSION      Override version tag (default: 7.3.5-local)"
        echo "  TARGET_BIN   Target binary path (default: /opt/homebrew/opt/cliproxyapi/bin/cliproxyapi)"
        echo "  PROD_CONFIG  Production config path (default: /opt/homebrew/etc/cliproxyapi.conf)"
        exit 0
        ;;
    *)
        log_err "Unknown command: ${COMMAND}. See '$0 help'."
        exit 1
        ;;
esac
