# localdev — Local Colima environment to review `local-path-provisioner` (SCSAUS-127)

A self-contained, **Kind-free** local setup for deploying `local-path-provisioner` (lpp)
and reviewing how it meets the S3DF technical requirements before migrating the
slaclab fork back to upstream.

- **JIRA:** [SCSAUS-127](https://jira.slac.stanford.edu/browse/SCSAUS-127) — *update local-path-provision*
- **Approach:** Colima's built-in **k3s** (containers via the Apple-`vz` VM) — not Kind, not Docker Desktop.
- **Stand-in for the shared FS:** a single host directory `/opt/sdf-data` on the one-node VM
  plays the role of the GPFS/Lustre mount that S3DF presents at the same path on every node.

> Companion design notes: [LOCAL_DEV_TESTING.md](../LOCAL_DEV_TESTING.md).

---

## Requirements this environment is built to review

| # | Requirement (from the ticket) | Mechanism here | Manifest |
|---|-------------------------------|----------------|----------|
| **R1** | StorageClass carries a **dynamic label** so NFD etc. can steer pods **off bad-FS nodes** | `nodeAffinityKey` SC param + node label + workload `nodeSelector` | [tests/r1-node-steering.yaml](tests/r1-node-steering.yaml) |
| **R2** | Share the **per-PVC subdir** `/sdf/data/X/<pvc>`, never the top-level `X` | lpp always provisions `<root>/<pv>_<ns>_<pvc>` | [tests/r2-subdir-retain.yaml](tests/r2-subdir-retain.yaml) |
| **R3** | Shared-FS **access modes** RWX / RWO / ROX (PR #183) | `sharedFileSystemPath` mode | [tests/r3-access-modes.yaml](tests/r3-access-modes.yaml) |
| **R4** | Migration safety: PVs on **`reclaimPolicy: Retain`** so an honored `Delete` can't wipe data | `sdf-data-shared` SC is `Retain` | [tests/r2-subdir-retain.yaml](tests/r2-subdir-retain.yaml) |

R1 is the **primary deliverable** and the one place stock upstream falls short — see [§ Review R1](#review-r1--pod-steering-the-gap).

---

## How it maps to production

```
  PRODUCTION (S3DF)                         LOCAL (this folder)
  ───────────────────                       ───────────────────
  many nodes, each mounting        ┌──►      1 colima node, one dir
  GPFS/Lustre at /sdf/data/...     │         /opt/sdf-data  (stands in for the mount)
                                   │
  lpp (shared-FS mode) makes       │         lpp (shared-FS mode) makes
  /sdf/data/X/<ns>_<pvc>/  ────────┘         /opt/sdf-data/<pv>_<ns>_<pvc>/
  and shares the SUBDIR, never the root      (identical behavior, one node)
```

Single-node is enough to verify R2/R3/R4 and to expose the R1 gap (empty PV
affinity). True multi-node *steering* needs ≥2 nodes — noted inline where relevant.

---

## Prerequisites

- macOS (Apple Silicon assumed below; Intel: drop the Rosetta flags).
- [Colima](https://github.com/abiosoft/colima) + `kubectl` + `docker` CLI:
  ```bash
  brew install colima kubectl docker
  ```

---

## Step 1 — Start the cluster (the k8s env)

```bash
# Apple Silicon, one-time: enables x86 emulation for amd64 images.
softwareupdate --install-rosetta --agree-to-license

# Single-node k3s inside a fast vz VM.
colima start --cpu 4 --memory 8 --disk 60 \
  --arch aarch64 --vm-type vz --vz-rosetta \
  --kubernetes

# CRITICAL: pin kubectl to the local cluster. NEVER target a prod context
# (sdf-monitoring, sdf-k8s01, coact, scs-iri, ...).
kubectl config use-context colima
kubectl config current-context        # must print: colima
kubectl get nodes                     # one node named "colima", Ready
```

> ⚠️ **Safety:** every command in this guide assumes `current-context == colima`.
> Re-check it after any context switch. Nothing here should ever run against prod.

---

## Step 2 — Create the shared "filesystem" and label the node (the fs)

```bash
# The directory that stands in for the shared mount.
colima ssh -- sudo mkdir -p /opt/sdf-data
colima ssh -- sudo chmod 0777 /opt/sdf-data

# The label R1 keys on: "this node has the shared FS mounted".
kubectl label node colima sdf.slac/data-mounted=true --overwrite
kubectl get node colima --show-labels | tr ',' '\n' | grep sdf.slac
```

---

## Step 3 — Deploy lpp + the StorageClasses

The manifests deploy **stock upstream** `rancher/local-path-provisioner:v0.0.35`
(the migration target) in shared-FS mode, plus two StorageClasses over the same root.

```bash
# Optional clean slate if older ad-hoc setups are still around:
kubectl delete ns local-path-storage local-path-upstream --ignore-not-found

kubectl apply -k localdev/manifests/

kubectl -n local-path-storage rollout status deploy/local-path-provisioner
kubectl get storageclass | grep sdf-data
kubectl -n local-path-storage logs -l app=local-path-provisioner --tail=20
```

What got created:

```
namespace/local-path-storage           the provisioner + RBAC + configmap + deployment
storageclass/sdf-data-shared           reclaimPolicy: Retain   (R2 + R4, RWX/ROX)
storageclass/sdf-data-scratch          reclaimPolicy: Delete   (force-delete the subdir, RWO)
configmap/local-path-config            sharedFileSystemPath: /opt/sdf-data, nodePathMap: []
```

---

## Step 4 — Review the requirements

Apply each test file, observe, then delete it. All test objects are labeled
`app=localdev-review` for easy cleanup.

### Review R2 — per-PVC subdir + R4 — Retain

```bash
kubectl apply -f localdev/tests/r2-subdir-retain.yaml
kubectl wait --for=jsonpath='{.status.phase}'=Bound pvc/r2-retain --timeout=60s

# R2: a per-PVC SUBDIR exists; the shared root is untouched.
colima ssh -- sudo ls -la /opt/sdf-data
#   drwxrwxr-x ... pvc-<uuid>_default_r2-retain/

# R4: the PV is Retain.
kubectl get pv -o custom-columns=\
'PV:.metadata.name,SC:.spec.storageClassName,RECLAIM:.spec.persistentVolumeReclaimPolicy'

# R4 proof: delete the claim, data SURVIVES, PV -> Released (not deleted).
kubectl delete -f localdev/tests/r2-subdir-retain.yaml
colima ssh -- sudo cat /opt/sdf-data/*_default_r2-retain/proof
```

### Review R3 — access modes (RWX / RWO / ROX)

```bash
kubectl apply -f localdev/tests/r3-access-modes.yaml
kubectl get pvc -l req=r3            # all three reach Bound in shared-FS mode

# RWX: both pods wrote to the same file.
kubectl exec r3-rwx-writer-1 -- cat /data/shared.txt   # contains pod-1 AND pod-2

kubectl delete -f localdev/tests/r3-access-modes.yaml
```

In `nodePathMap` mode the same RWX/ROX claims would be **rejected**
(`NodePath only supports ReadWriteOnce and ReadWriteOncePod` — [provisioner.go](../provisioner.go#L354)).
Shared-FS mode is what unlocks them.

### Review R1 — pod steering (the gap)

```bash
kubectl apply -f localdev/tests/r1-node-steering.yaml
kubectl wait --for=jsonpath='{.status.phase}'=Bound pvc/r1-steering --timeout=60s

# THE GAP: the PV has NO nodeAffinity even though the SC sets nodeAffinityKey.
kubectl get pv -o custom-columns=\
'PV:.metadata.name,SC:.spec.storageClassName,AFFINITY:.spec.nodeAffinity'
#   AFFINITY: <none>   <-- nodeAffinityKey is ignored in shared-FS mode

# Interim workaround: a workload-level nodeSelector keeps the consumer off
# unlabeled / bad-FS nodes. Prove the mechanism on one node:
kubectl label node colima sdf.slac/data-mounted-          # remove label
kubectl get pod r1-pod-with-nodeselector                  # -> Pending
kubectl label node colima sdf.slac/data-mounted=true      # restore
kubectl get pod r1-pod-with-nodeselector                  # -> Running

kubectl delete -f localdev/tests/r1-node-steering.yaml
```

**Why it matters / the fix.** In shared-FS mode lpp hard-codes
`nodeAffinity = nil` ([provisioner.go](../provisioner.go#L459)); the
`nodeAffinityKey` parameter is only consulted in the `nodePathMap` branch
([provisioner.go](../provisioner.go#L463)) — but that branch rejects RWX/ROX.
So today you must choose: shared-FS access modes **or** affinity, not both.
The ticket's deliverable is to make shared-FS mode honor `nodeAffinityKey` by
emitting an **`Exists`** affinity (require the label on *any* healthy node,
rather than pinning to one). Proposed shape:

```go
// provisioner.go, shared-FS branch (~L459)
if key, ok := storageClass.Parameters["nodeAffinityKey"]; ok && key != "" {
    nodeAffinity = &v1.VolumeNodeAffinity{Required: &v1.NodeSelector{
        NodeSelectorTerms: []v1.NodeSelectorTerm{{
            MatchExpressions: []v1.NodeSelectorRequirement{{
                Key: key, Operator: v1.NodeSelectorOpExists,
            }},
        }},
    }}
} else {
    nodeAffinity = nil   // unchanged default
}
```

Until that lands, the `nodeSelector` workaround above is the supported path.

---

## Migration safety (R4) — flip live PVs to Retain *before* swapping images

The fork gutted the delete code, so `reclaimPolicy: Delete` never actually
removed data. Stock upstream **will** honor it. Before pointing production at the
upstream image, set existing PVs to `Retain`:

```bash
kubectl get pv -o json \
  | jq -r '.items[] | select(.spec.storageClassName=="sdf-data-shared") | .metadata.name' \
  | xargs -I{} kubectl patch pv {} \
      -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'
```

---

## Reviewing the FORK instead of upstream

To compare behavior against the current fork image, build it locally and import
it into k3s (k3s uses containerd, not the Docker store):

```bash
# Build only the linux binary — ./scripts/build fails on macOS (darwin/arm).
source ./scripts/version
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
  -ldflags "-X main.VERSION=$VERSION -extldflags -static -s -w" \
  -o bin/local-path-provisioner-arm64

# Package + import (adjust to your image build; then load the tar into k3s):
docker save <your-fork-image> -o /tmp/lpp.tar
colima ssh -- sudo k3s ctr images import /tmp/lpp.tar
```

Then edit the `image:` in [manifests/00-provisioner.yaml](manifests/00-provisioner.yaml)
and re-apply. To run fork **and** upstream side-by-side, give the fork its own
namespace, a distinct `--provisioner-name`, and uniquely-named cluster-scoped
objects (StorageClass / ClusterRole / ClusterRoleBinding).

---

## Teardown

```bash
kubectl delete pods,pvc -l app=localdev-review --ignore-not-found
kubectl delete -k localdev/manifests/ --ignore-not-found
kubectl delete storageclass sdf-data-shared sdf-data-scratch --ignore-not-found
colima ssh -- sudo rm -rf /opt/sdf-data        # wipe the stand-in FS

# Stop or destroy the VM entirely:
colima stop
# colima delete
```

---

## Code references

| Behavior | Location |
|----------|----------|
| `isSharedFilesystem` decides mode | [provisioner.go](../provisioner.go#L264) |
| shared-FS vs nodePathMap are mutually exclusive | [provisioner.go](../provisioner.go#L272) |
| RWX/ROX rejected outside shared-FS mode | [provisioner.go](../provisioner.go#L354) |
| **`nodeAffinity = nil` in shared-FS (the R1 gap)** | [provisioner.go](../provisioner.go#L459) |
| `nodeAffinityKey` read only in nodePathMap branch | [provisioner.go](../provisioner.go#L463) |
| `Delete()` skipped when `Retain` | [provisioner.go](../provisioner.go#L524) |

- Upstream shared-FS feature: [rancher/local-path-provisioner#183](https://github.com/rancher/local-path-provisioner/pull/183)
