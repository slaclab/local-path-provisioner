# Local Dev & Migration Testing — local-path-provisioner (SCSAUS-127)

Distilled notes for validating the slaclab fork → upstream migration of `local-path-provisioner`
on a local machine, and for designing the storage features S3DF needs.

- **JIRA:** [SCSAUS-127](https://jira.slac.stanford.edu/browse/SCSAUS-127) — *update local-path-provision*
- **Goal:** Confirm stock upstream + correct config can replace the custom fork, so we stop maintaining a fork.
- **Target manifests:** `alpastor/local-path-provisioner/dev/lpp-upgrade` (Helm chart, `storageClassConfigs`).

---

## 1. Domain in one picture

`local-path-provisioner` (lpp) dynamically creates PersistentVolumes from a directory on a node.
At S3DF the "directory" is a **shared filesystem** (GPFS/Lustre) mounted at the same path
(`/sdf/data/...`) on every node, so a PV's data is reachable from any node.

```
        PVC ("I need 5Gi")                       Shared FS (same path on all nodes)
            │                                     /sdf/data/desc/lpp-upgrade/
            ▼                                        ├── <ns>_<pvc-A>/   ← per-PVC subdir
   ┌──────────────────┐   spawns helper pod         └── <ns>_<pvc-B>/
   │  lpp controller  │ ───────────────► mkdir/rm only inside the subdir,
   │ watches PVCs     │                  NEVER the shared root
   └──────────────────┘
            │ creates
            ▼
   PV (hostPath/local → /sdf/data/.../<ns>_<pvc>)
```

Key behavior: lpp **always** provisions a per-PVC subdirectory under the shared root and only ever
operates on that subdir — exactly what S3DF wants (share the subdir, not the top level).

---

## 2. Two provisioner modes (the central trade-off)

lpp has two mutually-exclusive config modes. They trade off the exact features we need:

```
 sharedFileSystemPath  ──► SHARED FS MODE          nodePathMap ──► NODEPATHMAP MODE
   ✅ subdir per PVC                                  ✅ subdir per PVC
   ✅ RWO + RWX + ROX        (provisioner.go:350)     ❌ RWO / RWOPod ONLY (rejects RWX/ROX)
   ✅ delete only subdir                              ✅ delete only subdir
   ❌ nodeAffinity = nil     (provisioner.go:453)     ✅ nodeAffinity SET (pins to ONE node)
      → nodeAffinityKey IGNORED
```

We need shared-FS mode for access modes and shared data, but that mode currently **drops**
the `nodeAffinityKey` — which is the heart of the ticket (steer pods off bad-FS nodes).

---

## 3. Local environment (Colima + Rosetta + k3s)

Single-node Kubernetes on Apple Silicon. A single node dir doubles as the "shared" filesystem.

```bash
# Apple Silicon, one-time
softwareupdate --install-rosetta --agree-to-license

colima start --cpu 4 --memory 8 --disk 60 \
  --arch aarch64 --vm-type vz --vz-rosetta \
  --kubernetes

kubectl config use-context colima        # ALWAYS use the local context, never prod
```

> ⚠️ **Never target prod.** Contexts like `sdf-monitoring` / `sdf-k8s01` are production clusters.
> Always `kubectl config use-context colima` first and verify with `kubectl config current-context`.

### Build on macOS (the `./scripts/build` gotcha)

`./scripts/build` fails on macOS because it forces `GOARCH=arm` while `GOOS` defaults to `darwin`
(`unsupported GOOS/GOARCH pair darwin/arm`). For the image you only need the linux binary:

```bash
source ./scripts/version
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
  -ldflags "-X main.VERSION=$VERSION -extldflags -static -s -w" \
  -o bin/local-path-provisioner-arm64
```

### Load an image into k3s (containerd, not Docker)

```bash
IMAGE="$(cat bin/latest_image)"
docker save "$IMAGE" -o /tmp/lpp.tar
colima ssh -- sudo k3s ctr images import /tmp/lpp.tar
```

---

## 4. Side-by-side setups (fork vs upstream)

Both provisioners run at once. They never collide because each StorageClass selects its provisioner
by a **distinct provisioner name**. Cluster-scoped objects (StorageClass, ClusterRole) get unique names.

```
ns: local-path-storage                 │  ns: local-path-upstream
image: <fork>:58ee708d-arm64           │  image: rancher/local-path-provisioner:v0.0.35
provisioner: rancher.io/local-path     │  provisioner: rancher.io/local-path-upstream
config: /opt/sdf-data                  │  config: /opt/sdf-data-upstream
StorageClass: sdf-shared (Delete)      │  StorageClass: sdf-shared-upstream (Retain)
```

Deploy upstream into its own namespace with `--provisioner-name rancher.io/local-path-upstream`
and `config.json = { "sharedFileSystemPath": "/opt/sdf-data-upstream", "nodePathMap": [] }`.

**Verified locally:** RWX PVCs bind on both; per-PVC subdir is created under the shared root
(not the root); upstream PV shows `reclaimPolicy: Retain`; in shared-FS mode **PV nodeAffinity is empty**
on both (confirms the feature-4 gap).

---

## 5. The four target features → lpp configuration

| # | Feature | Mechanism | Status |
|---|---------|-----------|--------|
| 1 | `use_subdir` — share subdir, never delete | `sharedFileSystemPath` + `reclaimPolicy: Retain` | ✅ works |
| 2 | `force_delete` — delete only the subdir | `reclaimPolicy: Delete` + `teardown: rm -rf $VOL_DIR` | ✅ works |
| 3 | `allow_once` — single writer (RWO) | PVC `accessModes: [ReadWriteOnce]` / `ReadWriteOncePod` | ✅ works |
| 4 | Node labelling — steer off bad-FS nodes | `nodeAffinityKey` StorageClass param | ❌ **needs code change** |

### Features 1–3: one provisioner, two StorageClasses

`teardown` is global and only touches `$VOL_DIR` (the subdir). `reclaimPolicy` alone decides whether
it ever runs, so two classes over the **same** shared root give both behaviors:

```yaml
# values for alpastor dev/lpp-upgrade (chart supports storageClassConfigs)
storageClassConfigs:
  sdf-data-shared:                 # F1: shared, never-delete, RWX
    storageClass: { create: true, reclaimPolicy: Retain, volumeBindingMode: WaitForFirstConsumer, defaultVolumeType: hostPath }
    sharedFileSystemPath: /sdf/data/desc/lpp-upgrade
    nodePathMap: []
  sdf-data-scratch:                # F2+F3: private, force-delete, RWO
    storageClass: { create: true, reclaimPolicy: Delete, volumeBindingMode: WaitForFirstConsumer, defaultVolumeType: hostPath }
    sharedFileSystemPath: /sdf/data/desc/lpp-upgrade
    nodePathMap: []

configmap:
  setup: |-
    #!/bin/sh
    set -eu
    mkdir -m 0775 -p "$VOL_DIR"     # create only the subdir
  teardown: |-
    #!/bin/sh
    set -eu
    rm -rf "$VOL_DIR"               # remove only the subdir (gated by reclaimPolicy)
```

- **Never delete** = `Retain` → `Delete()` is skipped entirely (provisioner.go:524).
- **Force delete** = `Delete` → teardown removes only `/sdf/data/.../<ns>_<pvc>`, never the root.
- **One writer** = `ReadWriteOnce` (or `ReadWriteOncePod` for a hard single-pod guarantee).

### Feature 4: the actual engineering work

In shared-FS mode lpp sets `nodeAffinity = nil`, so `nodeAffinityKey` is ignored and the Helm chart
doesn't even render it. Proposed change in `provisionFor` (provisioner.go ~L453): when shared-FS **and**
`nodeAffinityKey` is set, emit an **Exists** affinity so PVs require the label on healthy nodes
(instead of pinning to one node):

```go
if sharedFS {
    if key, ok := storageClass.Parameters["nodeAffinityKey"]; ok && key != "" {
        nodeAffinity = &v1.VolumeNodeAffinity{
            Required: &v1.NodeSelector{
                NodeSelectorTerms: []v1.NodeSelectorTerm{{
                    MatchExpressions: []v1.NodeSelectorRequirement{{
                        Key:      key,
                        Operator: v1.NodeSelectorOpExists,
                    }},
                }},
            },
        }
    } else {
        nodeAffinity = nil
    }
} else {
    // ...existing nodePathMap branch unchanged...
}
```

**Interim workaround (no code change):** add a workload-level `nodeSelector: { sdf.slac/data-mounted: "true" }`
so the consuming pod (and helper pod) avoid unlabeled nodes.

---

## 6. Migration safety (call-outs from the ticket)

- Production sets `reclaimPolicy: Delete` only to avoid PV proliferation; the fork **gutted the delete code**
  so data was never actually removed. Stock upstream **will** honor Delete and run `rm -rf` on the subdir.
- **Before** swapping to the upstream image, flip existing PVs to `Retain` so the now-active `Delete()`
  cannot wipe data:

  ```bash
  kubectl get pv -o json \
    | jq -r '.items[] | select(.spec.storageClassName=="sdf-data-shared") | .metadata.name' \
    | xargs -I{} kubectl patch pv {} -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'
  ```

---

## 7. Local test matrix

| Feature | Action | Expected |
|---------|--------|----------|
| 1 | RWX PVC on `sdf-data-shared` → write → delete PVC | PV `Released`; subdir + data still on disk; root intact |
| 2 | RWO PVC on `sdf-data-scratch` → write → delete PVC | subdir `rm -rf`'d; root intact |
| 3 | 2nd pod mounts same RWO/RWOPod PVC | second writer blocked |
| 4 | Unlabel node, schedule pod | **today:** still schedules (gap); **after fix:** PV affinity `Exists` keeps pod off unlabeled nodes |

Run T1/T2/T3 first (non-destructive aside from their own PVCs). T4 is the one that only passes
after the code change in §5.

---

## 8. Quick reference

```bash
# Always confirm the local context first
kubectl config current-context                 # must be: colima

# Inspect a provisioner
kubectl -n local-path-upstream get pods
kubectl -n local-path-upstream logs -l app=local-path-provisioner --tail=50

# See where data actually lands
colima ssh -- ls -laR /opt/sdf-data-upstream

# PV reclaim policy + affinity at a glance
kubectl get pv -o custom-columns=\
'PV:.metadata.name,SC:.spec.storageClassName,RECLAIM:.spec.persistentVolumeReclaimPolicy,AFFINITY:.spec.nodeAffinity'
```

### Key code locations
- `isSharedFilesystem` — [provisioner.go](provisioner.go#L264)
- Access-mode restriction (nodePathMap only) — [provisioner.go](provisioner.go#L350)
- `nodeAffinity = nil` in shared-FS mode (feature-4 gap) — [provisioner.go](provisioner.go#L453)
- Delete skipped when `Retain` — [provisioner.go](provisioner.go#L524)
- `pathPattern` rendering in chart (no `nodeAffinityKey`) — [storageclass.yaml](deploy/chart/local-path-provisioner/templates/storageclass.yaml#L30-L32)

---

## References
- Upstream shared-FS feature: [rancher/local-path-provisioner#183](https://github.com/rancher/local-path-provisioner/pull/183)
- Fork: [slaclab/local-path-provisioner](https://github.com/slaclab/local-path-provisioner)
- Related Kind-based plan: [UPLIFT_KIND_TEST_REPORT.md](UPLIFT_KIND_TEST_REPORT.md)
