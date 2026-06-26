# S3DF local-path-provisioner Dev E2E Test Suite

This directory contains end-to-end tests for validating the **upstream migration** of local-path-provisioner on real S3DF infrastructure.

## Purpose

The tests ensure that the **upstream provisioner + `sharedFileSystemPath` + `reclaimPolicy: Retain`** configuration works correctly on dev clusters with real `/sdf/data` mounts, before deploying to production.

Key goals:
- **Data preservation**: Verify that `reclaimPolicy: Retain` actually prevents data loss
- **Pod steering**: Confirm NFD labels + `nodeAffinityKey` work (SCSAUS-127 requirement)
- **Access modes**: Validate RWX/RWO/ROX on shared filesystems
- **Multiple StorageClasses**: Test coexistence of different provisioners/policies
- **Stress**: Ensure stability under concurrent operations
- **Upgrade path**: Confirm existing fork PVs remain valid after upstream migration

---

## Prerequisites

### 1. Dev Cluster Access

You must have a **dev/staging Kubernetes cluster** connected to S3DF's actual `/sdf/data` (GPFS/Lustre):

```bash
# Verify you're NOT pointing at production
kubectl config current-context   # should be dev, not sdf-monitoring

# Verify /sdf/data is mounted on nodes
kubectl get nodes -o wide
# then SSH into a node and check:
# ls -la /sdf/data
```

### 2. Deploy Upstream Provisioner

Use the helper script to deploy the upstream provisioner:

```bash
# Deploy with defaults (Retain, sdf.slac/data-mounted key, /sdf/data path)
./test/dev-e2e-deploy-upstream.sh

# Or customize:
./test/dev-e2e-deploy-upstream.sh \
  --namespace local-path-upstream \
  --provisioner-name rancher.io/local-path-upstream \
  --storage-class sdf-shared-upstream \
  --sdf-path /sdf/data \
  --reclaim-policy Retain \
  --node-affinity-key sdf.slac/data-mounted \
  --image rancher/local-path-provisioner:v0.0.35
```

Verify deployment:

```bash
kubectl -n local-path-upstream get pods
kubectl -n local-path-upstream logs -l app=local-path-provisioner | grep -E 'config|error|started'
kubectl get sc sdf-shared-upstream -o wide
```

### 3. Node Labels (Optional, for steering test)

If NFD is not running, manually label nodes with the health-check label:

```bash
# Label healthy nodes
kubectl label node <node-with-mounted-sdf> sdf.slac/data-mounted=true

# Unlabeled nodes simulate bad-FS scenarios
kubectl get nodes -L sdf.slac/data-mounted
```

---

## Running Tests

### All tests (recommended first run):

```bash
chmod +x test/dev-e2e-tests.sh
./test/dev-e2e-tests.sh all
```

### Specific test:

```bash
./test/dev-e2e-tests.sh data-preservation
./test/dev-e2e-tests.sh pod-steering
./test/dev-e2e-tests.sh access-modes
./test/dev-e2e-tests.sh multiple-storage-classes
./test/dev-e2e-tests.sh concurrent-creates
./test/dev-e2e-tests.sh upgrade-path
```

### Multiple specific tests:

```bash
./test/dev-e2e-tests.sh data-preservation pod-steering access-modes
```

---

## Test Descriptions

### 1. Data Preservation (`data-preservation`)

**What it tests:**
- PVC created with `reclaimPolicy: Retain`
- Data written to the volume
- PVC is deleted → PV should transition to `Released`
- **Data on shared filesystem must still exist** (not deleted by the provisioner)

**Why it matters:**
The fork gutted the `Delete()` function to prevent accidental data wiping. Upstream's `Delete()` actively runs `rm -rf`. The test verifies that `reclaimPolicy: Retain` stops Kubernetes from calling `Delete()` at all.

**Expected result:**
```
✓ PASS: PV reclaim policy is Retain
✓ PASS: Data was written to volume
✓ PASS: PV is in Released state (not deleted)
✓ PASS: Data file still exists on shared filesystem after PVC deletion
```

**If it fails:**
- Check `reclaimPolicy` on the StorageClass: must be `Retain`
- Verify the provisioner logs don't show deletion errors
- Check `/sdf/data` directly to see if subdirs were wiped

---

### 2. Pod Steering (`pod-steering`)

**What it tests:**
- A PVC is created with `nodeAffinityKey: sdf.slac/data-mounted`
- A pod requesting that PVC should land on nodes with the label
- Nodes without the label should be avoided

**Why it matters:**
SCSAUS-127: NFD detects bad filesystems and marks nodes. Pods should not land on those nodes.

**Expected result:**
```
✓ PASS: Found healthy node: node-1 (with label sdf.slac/data-mounted=true)
✓ PASS: Pod landed on healthy node: node-1
```

**If it fails or produces a warning:**
```
ℹ WARNING: Pod landed on node-2 (not constrained by nodeAffinityKey in sharedFS mode)
→ This is SCSAUS-127 gap: nodeAffinityKey is ignored when sharedFileSystemPath is used
```

This gap is **expected** in the current implementation. The code in `provisioner.go` sets `nodeAffinity = nil` for sharedFS mode. This is a known limitation being addressed separately.

---

### 3. Access Modes (`access-modes`)

**What it tests:**
- RWX (ReadWriteMany) — multiple pods can write simultaneously
- RWO (ReadWriteOnce) — single pod has write access
- ROX (ReadOnlyMany) — multiple pods can read

**Why it matters:**
Shared filesystems should support all three modes. The fork may have been limited.

**Expected result:**
```
✓ PASS: RWX access mode supported
✓ PASS: RWO access mode supported
✓ PASS: ROX access mode supported
```

---

### 4. Multiple Storage Classes (`multiple-storage-classes`)

**What it tests:**
- Multiple provisioners with different configurations coexist
- PVCs using different SCs provision independently

**Why it matters:**
The JIRA ticket mentions a disabled test for multiple storage classes. This verifies the gap is fixed.

**Expected result:**
```
✓ PASS: PVC multi-class-sdf-shared-upstream-xxx (SC: sdf-shared-upstream) provisioned successfully
```

**Note:** Requires at least two storage classes. If you only have one upstream class, extend the deployment script to create a second provisioner instance with a different path.

---

### 5. Concurrent Creates/Deletes (`concurrent-creates`)

**What it tests:**
- Creates 10 PVCs rapidly in parallel
- Verifies all bind successfully
- Deletes all concurrently
- Confirms cleanup

**Why it matters:**
Stress test — ensures the provisioner doesn't deadlock or lose state under load.

**Expected result:**
```
ℹ INFO: Creating 10 PVCs concurrently...
✓ PASS: At least 9 of 10 PVCs bound
ℹ INFO: Deleting all stress PVCs...
✓ PASS: All stress PVCs cleaned up
```

---

### 6. Upgrade Path (`upgrade-path`)

**What it tests:**
- Fork provisioner creates a PV
- Upstream provisioner creates a separate PV
- Both coexist without conflict
- Both have correct StorageClass reference

**Why it matters:**
Verifies the migration strategy: deploy upstream alongside fork, gradually migrate PVCs by updating their StorageClass.

**Expected result:**
```
ℹ INFO: Fork PV created: pvc-xxxx
✓ PASS: Fork PV uses correct storage class
ℹ INFO: Upstream PV created: pvc-yyyy
✓ PASS: Upstream PV uses correct storage class
✓ PASS: Fork PV still exists alongside upstream
✓ PASS: Upstream PV exists
```

---

## Test Results

Tests write to two files:

- **`test/dev-e2e-results.log`** — all output (pass, fail, info)
- **`test/dev-e2e-failures.log`** — only failures + error details

Example:
```bash
tail -f test/dev-e2e-results.log
grep FAIL test/dev-e2e-failures.log
```

---

## Interpreting Failures

### Common Issues

**Pod stuck `Pending`:**
- Check: `kubectl describe pvc <name>`
- Look for events indicating provisioner didn't claim the PVC
- Verify provisioner is running: `kubectl -n local-path-upstream get pods`
- Check provisioner logs for errors: `kubectl -n local-path-upstream logs -l app=local-path-provisioner`

**Data not found after deletion:**
- `reclaimPolicy: Delete` is still active on the StorageClass (should be `Retain`)
- The provisioner's `teardown` script ran (this is upstream's normal behavior)
- To fix: Change SC to `reclaimPolicy: Retain`

**Pod landed on wrong node (steering test):**
- Expected: pods constrained to healthy nodes only
- Actual: pod can land on any node
- Root cause: `nodeAffinityKey` is not used in `sharedFileSystemPath` mode
- This is **SCSAUS-127 gap** — acknowledged and tracked separately

**Multiple StorageClasses test skipped:**
- Only one sdf-shared class found
- Extend the deployment to create a second provisioner instance, or skip this test

---

## Cleanup

The tests auto-cleanup PVCs after each test. To manually clean:

```bash
# Remove all test PVCs
kubectl delete pvc -n default -l "pvc~=preserve-test|steering-test|access-test|multi-class|stress|upgrade" --wait=false

# Remove provisioner (if done)
kubectl delete ns local-path-upstream --wait=false
```

---

## Integration with CI

To run in CI/CD:

```bash
# Deploy provisioner to dev cluster
./test/dev-e2e-deploy-upstream.sh

# Run tests
./test/dev-e2e-tests.sh all

# Exit code 0 if all pass, 1 if any fail
echo $?
```

Example GitHub Actions snippet:
```yaml
- name: Deploy upstream provisioner
  run: ./test/dev-e2e-deploy-upstream.sh --namespace test-${{ github.run_id }}

- name: Run E2E tests
  run: ./test/dev-e2e-tests.sh all

- name: Upload results
  if: always()
  uses: actions/upload-artifact@v3
  with:
    name: e2e-results
    path: test/dev-e2e-*.log
```

---

## Known Gaps & Limitations

### 1. nodeAffinityKey Ignored in sharedFileSystemPath Mode

**Gap:** SCSAUS-127 requires pod steering based on NFD labels, but the current code forces `nodeAffinity = nil` for shared filesystems.

**Status:** Known limitation, tracked separately. Pods **will** land on any available node regardless of the `nodeAffinityKey` parameter.

**Workaround:** Use pod-level `nodeSelector` or `affinity` rules in your workloads until this is fixed in the provisioner code.

**Test behavior:** The `pod-steering` test will print a **WARNING** but not fail, since this is expected.

### 2. Multiple Storage Classes Test

**Requirement:** Two storage classes with different `sharedFileSystemPath` values to fully exercise this.

**Current state:** The deployment creates only one. To test this, deploy a second provisioner instance with a different path (e.g., `/sdf/scratch`).

### 3. Performance/Throughput Testing

This test suite focuses on **correctness**, not performance. For load/stress testing on real hardware, see separate performance test plan (if any).

---

## References

- JIRA Ticket: [SCSAUS-127](https://jira.slac.stanford.edu/browse/SCSAUS-127)
- Upstream PR: [rancher/local-path-provisioner#183](https://github.com/rancher/local-path-provisioner/pull/183) (sharedFileSystemPath feature)
- Repository: [slaclab/local-path-provisioner](https://github.com/slaclab/local-path-provisioner)

---

## Questions?

Contact: @amithm (or SCS-Platforms team)
