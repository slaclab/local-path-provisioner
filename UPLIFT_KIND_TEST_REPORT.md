# Kind-Based Validation Plan for Upstream Uplift

## Purpose

This report describes how to create a local Kubernetes environment with Kind and use it to validate an uplift of this fork of `local-path-provisioner` from the upstream Rancher repository. The goal is to prove that upstream changes can be merged without losing the fork-specific behavior needed for S3DF/slaclab infrastructure.

Kind should be treated as the first validation gate. It is strong for Kubernetes API behavior, manifests, image loading, provisioning logic, and multi-node scheduling. It does not replace final validation on an S3DF or staging cluster with the real filesystem, node labels, node lifecycle, and operational constraints.

## Repository Context

- Current fork remote: `git@github.com:slaclab/local-path-provisioner.git`
- Expected upstream remote: `https://github.com/rancher/local-path-provisioner.git`
- Default provisioner name: `rancher.io/local-path`
- Default namespace: `local-path-storage`
- Local e2e cluster: `test/testdata/kind-cluster.yaml`, with one control-plane node and two worker nodes
- Main e2e harness: `test/pod_test.go`
- Image handoff file for e2e: `bin/latest_image`

## Local Tooling

### Recommended Path: Make and Dapper

The repository already carries a reproducible build/test environment through `Makefile` and `Dockerfile.dapper`. The Make targets download `.dapper`, mount the Docker socket, and run the scripts inside a Linux container that installs Kind, kubectl, kustomize, buildx, Go, and golangci-lint.

Use this path first because it is closest to CI:

```bash
make ci
make e2e-test
```

`make ci` runs build, unit tests, validation, CI validation, and packaging. Packaging writes the built image name to `bin/latest_image`. `make e2e-test` reads that file and runs the Kind-backed e2e suite.

### Direct macOS Host Path

For a developer who wants to understand and run the pieces directly on macOS, install the local tools:

```bash
brew install kind kubectl kustomize go
```

Also ensure Docker Desktop or another Docker-compatible daemon is running. The repo script `scripts/package` invokes a `buildx` command directly, so the host path also needs a working buildx executable available on `PATH`. If that becomes awkward, prefer the Make/Dapper path above.

Direct script flow:

```bash
./scripts/build
./scripts/package
./scripts/e2e-test
```

The e2e script fails early if `bin/latest_image` does not exist, so always package before running it.

## Replicating S3DF Infrastructure with Kind

Kind runs real Kubernetes nodes as Docker containers on your laptop. Below is a step-by-step setup that approximates the S3DF cluster topology: multiple worker nodes with stable identity labels, dedicated local-path storage directories, and the provisioner configured the way it runs in production.

### 1. Install Kind

```bash
# macOS (Homebrew)
brew install kind

# or fetch the binary directly (Linux/macOS)
kind_version=$(curl -sL https://api.github.com/repos/kubernetes-sigs/kind/releases/latest | jq -r ".tag_name")
curl -Lo ./kind "https://kind.sigs.k8s.io/dl/${kind_version}/kind-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m)"
chmod +x ./kind && sudo mv ./kind /usr/local/bin/kind
```

Verify:

```bash
kind version
```

### 2. Create a Multi-Node Cluster with Extra Mounts

Save the following as `kind-s3df.yaml` (or adapt `test/testdata/kind-cluster.yaml`). The `extraMounts` entries simulate the host-local storage paths that exist on S3DF nodes (e.g. `/sdf/data`, `/sdf/scratch`). Adjust paths to match the actual S3DF layout you want to test.

```yaml
apiVersion: kind.x-k8s.io/v1alpha4
kind: Cluster
nodes:
  - role: control-plane
  - role: worker
    extraMounts:
      - hostPath: /tmp/s3df-node1-data
        containerPath: /sdf/data
      - hostPath: /tmp/s3df-node1-scratch
        containerPath: /sdf/scratch
  - role: worker
    extraMounts:
      - hostPath: /tmp/s3df-node2-data
        containerPath: /sdf/data
      - hostPath: /tmp/s3df-node2-scratch
        containerPath: /sdf/scratch
```

Create the cluster:

```bash
# Pre-create host directories so Kind does not complain
mkdir -p /tmp/s3df-node{1,2}-{data,scratch}

kind create cluster --name s3df-local --config kind-s3df.yaml --wait 120s
```

### 3. Label Nodes with Stable Identity

S3DF nodes use stable labels rather than ephemeral hostnames for volume affinity. Replicate this on Kind workers:

```bash
kubectl label node s3df-local-worker  slac.stanford.edu/node-id=sdf-node-001 --overwrite
kubectl label node s3df-local-worker2 slac.stanford.edu/node-id=sdf-node-002 --overwrite
```

### 4. Build and Load the Provisioner Image

```bash
./scripts/build
./scripts/package
kind load docker-image "$(cat bin/latest_image)" --name s3df-local
```

### 5. Deploy the Provisioner with S3DF-Style Config

Create a ConfigMap that mirrors the S3DF storage paths (adjust node names to the Kind worker hostnames or use `DEFAULT_PATH_FOR_NON_LISTED_NODES`):

```yaml
# s3df-config.yaml
kind: ConfigMap
apiVersion: v1
metadata:
  name: local-path-config
  namespace: local-path-storage
data:
  config.json: |-
    {
      "nodePathMap": [
        {
          "node": "DEFAULT_PATH_FOR_NON_LISTED_NODES",
          "paths": ["/sdf/data", "/sdf/scratch"]
        }
      ]
    }
  setup: |-
    #!/bin/sh
    set -eu
    mkdir -m 0777 -p "$VOL_DIR"
  teardown: |-
    #!/bin/sh
    set -eu
    rm -rf "$VOL_DIR"
  helperPod.yaml: |-
    apiVersion: v1
    kind: Pod
    metadata:
      name: helper-pod
    spec:
      priorityClassName: system-node-critical
      tolerations:
        - key: node.kubernetes.io/disk-pressure
          operator: Exists
          effect: NoSchedule
      containers:
      - name: helper-pod
        image: busybox
        imagePullPolicy: IfNotPresent
```

Deploy the provisioner using kustomize or direct manifests, overriding the image and config:

```bash
kubectl apply -f deploy/local-path-storage.yaml
kubectl -n local-path-storage delete configmap local-path-config
kubectl apply -f s3df-config.yaml
kubectl -n local-path-storage set image deployment/local-path-provisioner \
  local-path-provisioner="$(cat bin/latest_image)"
kubectl -n local-path-storage rollout status deployment/local-path-provisioner
```

### 6. Create a StorageClass with Stable Node Affinity

This mirrors how S3DF uses a stable label key instead of `kubernetes.io/hostname`:

```yaml
# s3df-storageclass.yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: s3df-local-path
provisioner: rancher.io/local-path
parameters:
  nodeAffinityKey: slac.stanford.edu/node-id
volumeBindingMode: WaitForFirstConsumer
reclaimPolicy: Delete
```

```bash
kubectl apply -f s3df-storageclass.yaml
```

### 7. Smoke Test a PVC

```yaml
# test-pvc.yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: s3df-test-claim
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: s3df-local-path
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: s3df-test-pod
spec:
  containers:
    - name: writer
      image: busybox
      command: ["sh", "-c", "echo s3df-ok > /data/proof && sleep 3600"]
      volumeMounts:
        - mountPath: /data
          name: vol
  volumes:
    - name: vol
      persistentVolumeClaim:
        claimName: s3df-test-claim
```

```bash
kubectl apply -f test-pvc.yaml
kubectl wait pod/s3df-test-pod --for=condition=Ready --timeout=60s
kubectl exec s3df-test-pod -- cat /data/proof   # expect: s3df-ok
```

### 8. Verify Node Affinity on the PV

```bash
kubectl get pv -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.nodeAffinity}{"\n"}{end}'
```

Expect the PV to reference `slac.stanford.edu/node-id` rather than `kubernetes.io/hostname`.

### 9. Cleanup

```bash
kubectl delete -f test-pvc.yaml
kind delete cluster --name s3df-local
rm -rf /tmp/s3df-node{1,2}-{data,scratch}
```

### Key Differences from Real S3DF

| Aspect | Kind replica | Real S3DF |
| --- | --- | --- |
| Filesystem | Temp dirs on your Mac via Docker mounts | Real NVMe/GPFS/Lustre paths |
| Node identity | Manually applied labels | Labels set by infrastructure automation |
| Network storage | Not available | May have shared NFS/GPFS |
| Performance | Local SSD / APFS | Production-grade storage |
| Node lifecycle | Containers, fast recreate | Bare-metal, longer lifecycle |

Kind proves provisioner logic, manifests, RBAC, node affinity, and path handling. It does not prove filesystem performance, actual shared-filesystem semantics, or infrastructure-automation interactions. Use S3DF staging for those.

## How the Kind E2E Harness Works

The e2e suite in `test/pod_test.go` performs this lifecycle:

1. Delete any existing default Kind cluster.
2. Create a new Kind cluster from `test/testdata/kind-cluster.yaml`.
3. Load the Docker image named in `TEST_IMAGE` into the Kind nodes.
4. For each test scenario, update kustomize image references, apply manifests, wait for readiness, and inspect PV/PVC behavior.
5. Delete the scenario manifests after each test.
6. Delete the Kind cluster when the suite finishes.

Because the suite uses the default Kind cluster name and deletes it, do not run multiple e2e suites in parallel on the same machine.

The command ultimately run by `scripts/e2e-test` is:

```bash
TEST_IMAGE="$(cat ./bin/latest_image)" go test -cover -v --tags=e2e -timeout 20m ./test/...
```

## Uplift Workflow

Use three working states so failures are easy to classify:

1. **Fork baseline**: current `slaclab` branch before the uplift.
2. **Upstream target**: the Rancher upstream commit or tag being pulled in.
3. **Uplift branch**: fork branch after merge/rebase/cherry-pick and conflict resolution.

Suggested Git setup:

```bash
git remote add upstream https://github.com/rancher/local-path-provisioner.git
git fetch origin
git fetch upstream
git checkout -b uplift/upstream-sync origin/main
```

Then merge or rebase the desired upstream target according to the team's Git policy. After conflicts are resolved, run the validation matrix below.

## Validation Matrix

| Gate | Branch or state | Command or check | Expected result |
| --- | --- | --- | --- |
| Fork baseline | Current fork before uplift | `make ci && make e2e-test` | Existing behavior passes locally |
| Upstream target | Clean upstream target, if practical | Build and run upstream tests | Establish whether upstream itself is healthy |
| Uplift compile | Uplift branch | `make ci` | Binaries, unit tests, lint, validation, and package pass |
| Uplift e2e | Uplift branch | `make e2e-test` | Kind scenarios pass using the uplift image |
| Manual smoke | Uplift branch Kind cluster or a fresh cluster | kubectl checks below | Provisioner, PVs, PVCs, logs, and cleanup look correct |
| S3DF/staging | Staging cluster | Team deployment process | Real infrastructure behavior is preserved |

## Automated Coverage to Preserve

The current e2e suite covers these important behaviors:

- Basic hostPath provisioning.
- Local volume provisioning.
- Default local volume configuration.
- Node affinity behavior.
- ReadWriteOncePod volume behavior.
- Security context compatibility.
- Subpath behavior.
- Custom `nodeAffinityKey` storage class parameter.
- Custom `pathPattern` storage class parameter.
- Unsafe path-pattern opt-out with `allowUnsafePathPattern`.
- Path traversal rejection for unsafe rendered paths.

There is also test data for multiple storage classes and shared filesystem configuration, but `xxTestPodWithMultipleStorageClasses` is currently disabled. Treat this as a coverage gap for the uplift unless it is intentionally re-enabled or replaced.

## Uplift-Sensitive Areas

Review these areas carefully during conflict resolution and code review:

- `go.mod`: Kubernetes and external provisioner library versions.
- `provisioner.go`: config parsing and canonicalization.
- `provisioner.go`: `getPathOnNode`, `isSharedFilesystem`, and storage-class-specific config selection.
- `provisioner.go`: `pathFromPattern`, `allowUnsafePathPattern`, and traversal checks.
- `provisioner.go`: `Provision` and deletion/cleanup paths.
- `provisioner.go`: helper pod creation, setup, teardown, and config refresh behavior.
- `deploy/local-path-storage.yaml`: RBAC, service account, deployment args, ConfigMap layout, and storage class defaults.
- `.github/workflows/build.yml`: slaclab image registry, GHCR publishing, and multi-platform image behavior.

## Manual Smoke Checks

After `make e2e-test`, the cluster is deleted automatically. For manual inspection, either run targeted commands before cleanup while debugging a failed test, or create a separate Kind cluster and deploy the built image manually.

Useful checks during a live Kind run:

```bash
kubectl -n local-path-storage get pods
kubectl -n local-path-storage logs -l app=local-path-provisioner --tail=100
kubectl get storageclass
kubectl get pvc,pv -A
kubectl describe pv <pv-name>
kubectl get pods -A | grep helper || true
```

For S3DF-like stable node identity checks, label a Kind worker with the same style of stable key used by the target StorageClass, then verify the resulting PV node affinity uses that key and value:

```bash
kubectl label node kind-worker test.example.com/stable-id=my-stable-node --overwrite
kubectl get pv -o jsonpath='{.items[0].spec.nodeAffinity.required.nodeSelectorTerms[0].matchExpressions[0]}'
```

## Success Criteria

The uplift is ready for staging when all of the following are true:

- Fork baseline result is known before the uplift.
- Uplift branch passes `make ci`.
- Uplift branch passes `make e2e-test`.
- Provisioner logs do not show new recurring errors or permission failures.
- Custom `nodeAffinityKey` behavior still works.
- Custom `pathPattern` validation and opt-out behavior still work.
- Shared filesystem and multiple-storage-class behavior has either explicit test coverage or a documented manual validation result.
- Deploy manifests and CI image publishing still point to the intended slaclab/GHCR targets.
- S3DF or staging validation confirms behavior that Kind cannot model.

## Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `latest_image not found` | `scripts/e2e-test` ran before packaging | Run `make ci` or `./scripts/package` first |
| Kind cannot create cluster | Docker daemon is not running or not reachable | Start Docker Desktop and retry |
| Image pull errors in Kind | Image was not loaded or kustomize points to the wrong tag | Confirm `cat bin/latest_image` and rerun e2e image load flow |
| Cluster already exists | A previous Kind run was interrupted | Run `kind delete cluster`, then rerun the suite |
| RBAC forbidden errors | Upstream changed permissions or API usage | Compare `deploy/local-path-storage.yaml` with upstream and add only required permissions |
| PVC remains pending | Provisioning failed, node affinity mismatch, or path/config error | Check PVC events and provisioner logs |
| Manual checks impossible after e2e | The suite deleted the cluster by design | Reproduce with a targeted run or a separate manual Kind cluster |

## Recommended Next Improvements

- Add a short developer section in `README.md` that links to this report.
- Re-enable or replace the disabled multiple-storage-class e2e test.
- Add a small S3DF staging checklist once the exact production node labels, filesystem paths, and deployment process are confirmed.