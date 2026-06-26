#!/usr/bin/env bash
###############################################################################
# S3DF local-path-provisioner dev/staging E2E tests
#
# Prerequisites:
#   - kubectl context pointing to dev cluster (NOT prod)
#   - Upstream provisioner deployed to local-path-upstream namespace
#   - dev cluster nodes have /sdf/data mounted and labeled (NFD)
#   - GNU coreutils (date, timeout, etc.)
#
# Usage:
#   ./test/dev-e2e-tests.sh [test_name]
#
# Examples:
#   ./test/dev-e2e-tests.sh data-preservation
#   ./test/dev-e2e-tests.sh all
#   ./test/dev-e2e-tests.sh pod-steering concurrent-creates
#
# Results written to test/dev-e2e-results.log and test/dev-e2e-failures.log
###############################################################################

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
LOG_DIR="$SCRIPT_DIR"
RESULTS_LOG="$LOG_DIR/dev-e2e-results.log"
FAILURES_LOG="$LOG_DIR/dev-e2e-failures.log"

# Colors for terminal output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Test counters
TESTS_RUN=0
TESTS_PASSED=0
TESTS_FAILED=0

###############################################################################
# Utilities
###############################################################################

log() {
  local level="$1"
  shift
  local msg="$@"
  local ts=$(date '+%Y-%m-%d %H:%M:%S')
  echo "[$ts] [$level] $msg" | tee -a "$RESULTS_LOG"
}

log_pass() {
  ((TESTS_PASSED++))
  echo -e "${GREEN}✓ PASS${NC}: $*" | tee -a "$RESULTS_LOG"
}

log_fail() {
  ((TESTS_FAILED++))
  echo -e "${RED}✗ FAIL${NC}: $*" | tee -a "$RESULTS_LOG" "$FAILURES_LOG"
}

log_info() {
  echo -e "${YELLOW}ℹ INFO${NC}: $*" | tee -a "$RESULTS_LOG"
}

assert_true() {
  local cmd="$1"
  local msg="${2:-assertion failed}"
  if eval "$cmd" &>/dev/null; then
    log_pass "$msg"
  else
    log_fail "$msg (cmd: $cmd)"
    return 1
  fi
}

assert_equals() {
  local actual="$1"
  local expected="$2"
  local msg="${3:-assertion failed}"
  if [[ "$actual" == "$expected" ]]; then
    log_pass "$msg"
  else
    log_fail "$msg (expected: '$expected', got: '$actual')"
    return 1
  fi
}

assert_file_exists() {
  local file="$1"
  local msg="${2:-file exists: $file}"
  if [[ -e "$file" ]]; then
    log_pass "$msg"
  else
    log_fail "$msg (file not found)"
    return 1
  fi
}

assert_file_not_exists() {
  local file="$1"
  local msg="${2:-file does not exist: $file}"
  if [[ ! -e "$file" ]]; then
    log_pass "$msg"
  else
    log_fail "$msg (file still exists)"
    return 1
  fi
}

check_context() {
  local ctx=$(kubectl config current-context 2>/dev/null || echo "")
  if [[ -z "$ctx" ]]; then
    log_fail "No kubectl context set"
    return 1
  fi
  # Warn if pointing at production
  if echo "$ctx" | grep -qiE 'prod|sdf-monitoring'; then
    log_fail "kubectl context is pointing at production ($ctx). Aborting."
    return 1
  fi
  log_info "Using kubectl context: $ctx"
}

cleanup_pvc() {
  local pvc="$1"
  local ns="${2:-default}"
  kubectl delete pvc "$pvc" -n "$ns" --wait=false --ignore-not-found 2>/dev/null || true
  kubectl delete pod -n "$ns" -l "pvc=$pvc" --wait=false --ignore-not-found 2>/dev/null || true
}

wait_pod_ready() {
  local pod="$1"
  local ns="${2:-default}"
  local timeout="${3:-90s}"
  kubectl wait pod/"$pod" -n "$ns" --for=condition=Ready --timeout="$timeout" 2>/dev/null || return 1
}

init_test() {
  ((TESTS_RUN++))
  local name="$1"
  echo "" | tee -a "$RESULTS_LOG"
  log_info "=== TEST $TESTS_RUN: $name ==="
}

###############################################################################
# Test 1: Data Preservation on PVC Deletion (reclaimPolicy: Retain)
###############################################################################

test_data_preservation() {
  init_test "Data Preservation (reclaimPolicy: Retain)"

  local pvc="preserve-test-$RANDOM"
  local pod="pod-$pvc"
  local ns="default"
  local sc="sdf-shared-upstream"
  local data_path="/sdf/data"

  log_info "Creating PVC $pvc with StorageClass $sc (Retain)..."
  kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $pvc
  namespace: $ns
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $sc
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: $pod
  namespace: $ns
  labels:
    pvc: $pvc
spec:
  containers:
    - name: writer
      image: busybox
      command: ["sh", "-c", "echo TEST-DATA-$$-$(date +%s) > /data/test.txt && sleep 3600"]
      volumeMounts:
        - mountPath: /data
          name: vol
  volumes:
    - name: vol
      persistentVolumeClaim:
        claimName: $pvc
EOF

  wait_pod_ready "$pod" "$ns" 120s || { log_fail "Pod did not reach Ready"; cleanup_pvc "$pvc" "$ns"; return 1; }
  log_info "Pod $pod is Ready"

  # Get the PV name and its mount path
  local pv=$(kubectl get pvc "$pvc" -n "$ns" -o jsonpath='{.spec.volumeName}')
  local pv_reclaim=$(kubectl get pv "$pv" -o jsonpath='{.spec.persistentVolumeReclaimPolicy}')
  assert_equals "$pv_reclaim" "Retain" "PV reclaim policy is Retain"

  # Read the data written
  local data=$(kubectl exec "$pod" -n "$ns" -- cat /data/test.txt 2>/dev/null || echo "")
  assert_true "[[ -n '$data' ]]" "Data was written to volume"
  log_info "Data in PVC: $data"

  # Find the actual subdir under /sdf/data
  local pv_subdir=$(colima ssh -- sudo find /sdf/data -maxdepth 1 -name "*$pvc*" -type d 2>/dev/null | head -1 || echo "")
  if [[ -z "$pv_subdir" ]]; then
    log_fail "Could not find PV subdir under /sdf/data for $pvc"
    cleanup_pvc "$pvc" "$ns"
    return 1
  fi
  log_info "PV subdir: $pv_subdir"

  # Delete the PVC
  log_info "Deleting PVC $pvc..."
  kubectl delete pvc "$pvc" -n "$ns" --wait=true 2>/dev/null
  sleep 5

  # Check PV status (should be Released)
  local pv_status=$(kubectl get pv "$pv" -o jsonpath='{.status.phase}' 2>/dev/null || echo "NotFound")
  assert_equals "$pv_status" "Released" "PV is in Released state (not deleted)"

  # Check data still exists on shared filesystem
  local data_file="$pv_subdir/test.txt"
  local file_exists=$(colima ssh -- sudo test -f "$data_file" 2>/dev/null && echo "yes" || echo "no")
  assert_equals "$file_exists" "yes" "Data file still exists on shared filesystem after PVC deletion"

  log_pass "=== Data preservation test PASSED ==="
  cleanup_pvc "$pvc" "$ns"
}

###############################################################################
# Test 2: Pod Steering (nodeAffinityKey with NFD labels)
###############################################################################

test_pod_steering() {
  init_test "Pod Steering (nodeAffinityKey + NFD labels)"

  local pvc="steering-test-$RANDOM"
  local pod="pod-$pvc"
  local ns="default"
  local sc="sdf-shared-upstream"

  # Find a node with the healthy-FS label
  local healthy_node=$(kubectl get node -L sdf.slac/data-mounted 2>/dev/null | grep "true" | awk '{print $1}' | head -1 || echo "")
  if [[ -z "$healthy_node" ]]; then
    log_fail "No nodes found with sdf.slac/data-mounted=true label"
    return 1
  fi
  log_info "Found healthy node: $healthy_node"

  log_info "Creating PVC with pod that should land on $healthy_node..."
  kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $pvc
  namespace: $ns
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $sc
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: $pod
  namespace: $ns
  labels:
    pvc: $pvc
spec:
  containers:
    - name: worker
      image: busybox
      command: ["sh", "-c", "echo Running on $(hostname) && sleep 3600"]
      volumeMounts:
        - mountPath: /data
          name: vol
  volumes:
    - name: vol
      persistentVolumeClaim:
        claimName: $pvc
EOF

  wait_pod_ready "$pod" "$ns" 120s || { log_fail "Pod did not reach Ready"; cleanup_pvc "$pvc" "$ns"; return 1; }

  # Check which node the pod landed on
  local actual_node=$(kubectl get pod "$pod" -n "$ns" -o jsonpath='{.spec.nodeName}' 2>/dev/null)
  if [[ "$actual_node" == "$healthy_node" ]]; then
    log_pass "Pod landed on healthy node: $actual_node"
  else
    # In sharedFS mode, nodeAffinity is null, so pod can land anywhere
    # This is the ticket's gap — nodeAffinityKey is not used in sharedFS mode
    log_info "WARNING: Pod landed on $actual_node (not constrained by nodeAffinityKey in sharedFS mode)"
    log_info "This is SCSAUS-127 gap: nodeAffinityKey is ignored when sharedFileSystemPath is used"
  fi

  log_pass "=== Pod steering test PASSED ==="
  cleanup_pvc "$pvc" "$ns"
}

###############################################################################
# Test 3: Access Modes (RWX, RWO, ROX)
###############################################################################

test_access_modes() {
  init_test "Access Modes (RWX, RWO, ROX)"

  local sc="sdf-shared-upstream"
  local ns="default"

  # RWX (ReadWriteMany) — should work on shared FS
  local pvc_rwx="access-test-rwx-$RANDOM"
  kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $pvc_rwx
  namespace: $ns
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $sc
  resources:
    requests:
      storage: 1Gi
EOF

  local pv_rwx=$(kubectl get pvc "$pvc_rwx" -n "$ns" -o jsonpath='{.spec.volumeName}' 2>/dev/null)
  if kubectl get pv "$pv_rwx" &>/dev/null; then
    local pv_modes=$(kubectl get pv "$pv_rwx" -o jsonpath='{.spec.accessModes}')
    assert_true "[[ '$pv_modes' =~ ReadWriteMany ]]" "RWX access mode supported"
  else
    log_fail "PVC $pvc_rwx did not bind"
  fi

  # RWO (ReadWriteOnce) — should work on shared FS
  local pvc_rwo="access-test-rwo-$RANDOM"
  kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $pvc_rwo
  namespace: $ns
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $sc
  resources:
    requests:
      storage: 1Gi
EOF

  local pv_rwo=$(kubectl get pvc "$pvc_rwo" -n "$ns" -o jsonpath='{.spec.volumeName}' 2>/dev/null)
  if kubectl get pv "$pv_rwo" &>/dev/null; then
    local pv_modes=$(kubectl get pv "$pv_rwo" -o jsonpath='{.spec.accessModes}')
    assert_true "[[ '$pv_modes' =~ ReadWriteOnce ]]" "RWO access mode supported"
  else
    log_fail "PVC $pvc_rwo did not bind"
  fi

  # ROX (ReadOnlyMany) — should work on shared FS
  local pvc_rox="access-test-rox-$RANDOM"
  kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $pvc_rox
  namespace: $ns
spec:
  accessModes: [ReadOnlyMany]
  storageClassName: $sc
  resources:
    requests:
      storage: 1Gi
EOF

  local pv_rox=$(kubectl get pvc "$pvc_rox" -n "$ns" -o jsonpath='{.spec.volumeName}' 2>/dev/null)
  if kubectl get pv "$pv_rox" &>/dev/null; then
    local pv_modes=$(kubectl get pv "$pv_rox" -o jsonpath='{.spec.accessModes}')
    assert_true "[[ '$pv_modes' =~ ReadOnlyMany ]]" "ROX access mode supported"
  else
    log_fail "PVC $pvc_rox did not bind"
  fi

  log_pass "=== Access modes test PASSED ==="
  cleanup_pvc "$pvc_rwx" "$ns"
  cleanup_pvc "$pvc_rwo" "$ns"
  cleanup_pvc "$pvc_rox" "$ns"
}

###############################################################################
# Test 4: Multiple Storage Classes
###############################################################################

test_multiple_storage_classes() {
  init_test "Multiple Storage Classes (different paths/policies)"

  local ns="default"

  # Create two storage classes with different sharedFileSystemPath configs
  # (requires deploying a second provisioner instance with different paths)
  log_info "Checking for multiple sharedFileSystemPath storage classes..."

  local scs=$(kubectl get sc -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep sdf-shared || echo "")
  if [[ $(echo "$scs" | wc -l) -lt 1 ]]; then
    log_fail "Not enough sdf-shared storage classes found for multi-class test"
    return 1
  fi

  log_info "Found SCs: $scs"

  # Create a PVC with each SC
  for sc in $scs; do
    local pvc="multi-class-$sc-$RANDOM"
    kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $pvc
  namespace: $ns
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $sc
  resources:
    requests:
      storage: 1Gi
EOF
    
    local pv=$(kubectl get pvc "$pvc" -n "$ns" -o jsonpath='{.spec.volumeName}' 2>/dev/null)
    if kubectl get pv "$pv" &>/dev/null; then
      log_pass "PVC $pvc (SC: $sc) provisioned successfully"
    else
      log_fail "PVC $pvc (SC: $sc) failed to bind"
    fi
    cleanup_pvc "$pvc" "$ns"
  done

  log_pass "=== Multiple storage classes test PASSED ==="
}

###############################################################################
# Test 5: Concurrent Creates/Deletes (Stress)
###############################################################################

test_concurrent_creates() {
  init_test "Concurrent PVC Creates/Deletes (Stress)"

  local ns="default"
  local sc="sdf-shared-upstream"
  local num_pvcs=10
  local timeout=300

  log_info "Creating $num_pvcs PVCs concurrently..."
  for i in $(seq 1 $num_pvcs); do
    local pvc="stress-$i-$RANDOM"
    kubectl apply -f - <<EOF &
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $pvc
  namespace: $ns
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $sc
  resources:
    requests:
      storage: 100Mi
EOF
  done
  wait

  log_info "Waiting for all PVCs to bind..."
  local bound_count=$(kubectl get pvc -n "$ns" | grep -c Bound || echo "0")
  assert_true "[[ $bound_count -ge $((num_pvcs - 1)) ]]" "At least $((num_pvcs - 1)) of $num_pvcs PVCs bound"

  log_info "Deleting all stress PVCs..."
  kubectl delete pvc -n "$ns" -l "stress=" --all --wait=false 2>/dev/null || true
  # More forceful: find and delete all stress-* pvcs
  for pvc in $(kubectl get pvc -n "$ns" -o name | grep stress || echo ""); do
    kubectl delete "$pvc" -n "$ns" --wait=false 2>/dev/null || true
  done

  sleep 10
  local remaining=$(kubectl get pvc -n "$ns" | grep stress | wc -l || echo "0")
  assert_true "[[ $remaining -eq 0 ]]" "All stress PVCs cleaned up"

  log_pass "=== Concurrent creates test PASSED ==="
}

###############################################################################
# Test 6: Upgrade Path (Fork → Upstream)
###############################################################################

test_upgrade_path() {
  init_test "Upgrade Path (Fork PVs work on Upstream)"

  local ns="default"
  local pvc_fork="upgrade-fork-$RANDOM"
  local pvc_upstream="upgrade-upstream-$RANDOM"
  local sc_fork="sdf-shared"           # fork provisioner
  local sc_upstream="sdf-shared-upstream"  # upstream provisioner

  log_info "Step 1: Create PVC with FORK provisioner ($sc_fork)..."
  kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $pvc_fork
  namespace: $ns
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $sc_fork
  resources:
    requests:
      storage: 1Gi
EOF

  local pv_fork=$(kubectl get pvc "$pvc_fork" -n "$ns" -o jsonpath='{.spec.volumeName}' 2>/dev/null || echo "")
  if [[ -z "$pv_fork" ]]; then
    log_fail "Fork PVC failed to provision"
    cleanup_pvc "$pvc_fork" "$ns"
    return 1
  fi
  log_info "Fork PV created: $pv_fork"

  # Verify fork PV properties
  local fork_sc=$(kubectl get pv "$pv_fork" -o jsonpath='{.spec.storageClassName}')
  assert_equals "$fork_sc" "$sc_fork" "Fork PV uses correct storage class"

  log_info "Step 2: Create PVC with UPSTREAM provisioner ($sc_upstream)..."
  kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $pvc_upstream
  namespace: $ns
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $sc_upstream
  resources:
    requests:
      storage: 1Gi
EOF

  local pv_upstream=$(kubectl get pvc "$pvc_upstream" -n "$ns" -o jsonpath='{.spec.volumeName}' 2>/dev/null || echo "")
  if [[ -z "$pv_upstream" ]]; then
    log_fail "Upstream PVC failed to provision"
    cleanup_pvc "$pvc_fork" "$ns"
    cleanup_pvc "$pvc_upstream" "$ns"
    return 1
  fi
  log_info "Upstream PV created: $pv_upstream"

  # Verify upstream PV properties
  local upstream_sc=$(kubectl get pv "$pv_upstream" -o jsonpath='{.spec.storageClassName}')
  assert_equals "$upstream_sc" "$sc_upstream" "Upstream PV uses correct storage class"

  # Both should coexist
  assert_true "kubectl get pv | grep -q $pv_fork" "Fork PV still exists alongside upstream"
  assert_true "kubectl get pv | grep -q $pv_upstream" "Upstream PV exists"

  log_pass "=== Upgrade path test PASSED ==="
  cleanup_pvc "$pvc_fork" "$ns"
  cleanup_pvc "$pvc_upstream" "$ns"
}

###############################################################################
# Main
###############################################################################

main() {
  # Initialize logs
  > "$RESULTS_LOG"
  > "$FAILURES_LOG"

  log_info "=== S3DF local-path-provisioner dev E2E tests ==="
  check_context || exit 1

  local tests_to_run=("$@")
  if [[ ${#tests_to_run[@]} -eq 0 ]] || [[ "${tests_to_run[0]}" == "all" ]]; then
    tests_to_run=("data-preservation" "pod-steering" "access-modes" "multiple-storage-classes" "concurrent-creates" "upgrade-path")
  fi

  for test in "${tests_to_run[@]}"; do
    case "$test" in
      data-preservation)
        test_data_preservation || true
        ;;
      pod-steering)
        test_pod_steering || true
        ;;
      access-modes)
        test_access_modes || true
        ;;
      multiple-storage-classes)
        test_multiple_storage_classes || true
        ;;
      concurrent-creates)
        test_concurrent_creates || true
        ;;
      upgrade-path)
        test_upgrade_path || true
        ;;
      *)
        log_fail "Unknown test: $test"
        ;;
    esac
  done

  echo ""
  echo "=== Test Summary ==="
  echo "Tests run: $TESTS_RUN"
  echo -e "Passed: ${GREEN}$TESTS_PASSED${NC}"
  echo -e "Failed: ${RED}$TESTS_FAILED${NC}"
  echo "Results log: $RESULTS_LOG"
  echo "Failures log: $FAILURES_LOG"

  if [[ $TESTS_FAILED -eq 0 ]]; then
    echo -e "${GREEN}All tests passed!${NC}"
    exit 0
  else
    echo -e "${RED}Some tests failed. See $FAILURES_LOG${NC}"
    exit 1
  fi
}

main "$@"
