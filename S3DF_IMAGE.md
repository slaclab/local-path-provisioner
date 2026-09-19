# S3DF image contract

This image integrates upstream `rancher/local-path-provisioner` through
`9fa2a62eda441935d28b1d597aa5a3c665481d65` (v0.0.37 plus the subsequent upstream
updates). It is not interchangeable with the stock Rancher image.

## Required behavior

| Requirement | Image behavior |
|---|---|
| R1: retain filesystem data | `Delete` logs the PV name and path, touches neither nodes nor storage configuration, creates no Pods, and returns nil. The external provisioner controller then deletes the PV API object. |
| Reclaim policy | The StorageClass policy is passed through unchanged. There is no `disableDelete` setting or policy override. |
| R2: ConfigMap-only steering | `nodeAffinityKey` is part of the same configuration as the path. StorageClass parameters and steering annotations cannot supply it. |
| Shared affinity | A configured key emits one `Exists` expression without values. An absent key emits no affinity and logs a warning. |
| Node-local affinity | `In` with the selected node's label value; the default key remains `kubernetes.io/hostname`. |
| R3: facility root | Every shared PV and setup helper uses `sharedFileSystemPath` verbatim, with no claim-specific subdirectory. Both shared hostPath types are `Directory`. |
| Setup safety | Shared helpers mount the share itself, not its parent, read-only. They cannot redirect this mount with a custom mount path or subPath. |
| Node-local paths | Upstream per-PV subdirectories, path validation, and ownership/permission patterns remain available. Data deletion is disabled in this mode too. |

The startup log contains:

```text
S3DF data-preservation mode: filesystem deletion is permanently disabled; Delete reclaims PV API objects only
```

The delete-action helper branch and delete-time node lookup are removed.
Non-create helper actions are rejected, and teardown is not even mounted into
setup helpers. `teardownCommand` remains parseable for compatibility but has no
execution path. Changing it cannot enable deletion.

## Instance configuration

Use one Deployment, ConfigMap, StorageClass, and pre-existing shared path per
facility instance:

```json
{
  "sharedFileSystemPath": "/sdf/data/lcls",
  "nodePathMap": [],
  "nodeAffinityKey": "edu.stanford.slac.sdf.storage/sdf-data-lcls"
}
```

The Helm values have the same top-level names. For compatibility, the existing
`storageClassConfigs` map also carries the key per entry; keys do not implicitly
inherit from the top-level config in that mode. The Helm template rejects the
obsolete `storageClass.nodeAffinityKey` and StorageClass steering annotation.

Keys must be valid Kubernetes label keys. Shared paths must be absolute and not
the filesystem root. Configuration reloads preserve the last valid configuration
on error and affect only future PVs, never existing PVs. Provisioning and helper
template reads are synchronized with reloads.

`Exists` ignores label values. Both `"true"` and `"false"` satisfy it; an unhealthy
node must have the key removed. `Directory` checks that the directory exists, not
that the intended filesystem is mounted there. Accurate mount-health labels are
still essential, particularly when an unmounted directory remains on local disk.
Removing a label does not evict running consumers.

## Hazard decisions

| Hazard | Decision |
|---|---|
| H1: helpers on unmounted nodes | Shared helpers require the configured label key. Each existing non-empty required-affinity term is intersected with `Exists`; other scheduling constraints are preserved. `nodeName` cannot bypass this. |
| H2: implicit directory creation | Shared helpers and PVs use `Directory`. The helper mounts the exact root read-only, so setup cannot fabricate the share under an existing parent. |
| H3: shared writers | RWX/ROX/RWO remain supported. All claims see the same data; capacity is advisory, with no per-claim isolation or fencing. Applications own writer coordination. |
| H4: subdirectory/permission patterns | Shared provisioning rejects `pathPattern`, `allowUnsafePathPattern`, `userPattern`, `groupPattern`, and `permPattern` in parameters or annotations, including empty values. The last three are new upstream functionality that must not change facility-root ownership or permissions. |
| H5: PV ownership annotation | Existing-PV migration must preserve `pv.kubernetes.io/provisioned-by` exactly. Without it, the controller may not own or reclaim the PV. This is an infrastructure migration requirement, not an image rewrite. |
| H6: queued deletions after a freeze | On restart, owned Released/Delete PVs can be reclaimed as API objects without any filesystem helper. R1 applies regardless of current path configuration or whether a StorageClass was removed. |

The helper client uses the Kubernetes clientset interface so fake-client tests
exercise the real provisioning methods and external controller. A rejected helper
template reload keeps the last valid template instead of replacing it with nil.
These are deliberate testability and reload-safety changes alongside R1-R3/H1.

## Source checks and manual image build

No GitHub workflow customization or cluster deployment is required. The source
tests use the existing Go runner and Kubernetes fake clientset. The Helm-tagged
tests use an installed Helm executable and render locally only:

```sh
go test -race -tags=helm .
go test -tags=e2e -run '^$' ./test
```

The second command only compiles the existing e2e harness; it runs no cluster tests.

The regressions cover deletion across shared/node-local/custom-teardown configs,
actual PV API reclamation by the vendored controller, reclaim-policy passthrough,
ConfigMap precedence and reloads, shared paths and hostPath types, helper
scheduling, obsolete inputs, and preservation of node-local behavior. The default
setup script is exercised against a seeded temporary directory and its existing
permissions.

Build static Linux binaries with the compiler version pinned in `Dockerfile`,
using the vendored dependencies. For example, in a matching Go environment:

```sh
IMAGE="ghcr.io/slaclab/local-path-provisioner:${TAG}"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -trimpath -buildvcs=false \
  -ldflags "-X main.VERSION=${TAG} -s -w" -o bin/local-path-provisioner-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -mod=vendor -trimpath -buildvcs=false \
  -ldflags "-X main.VERSION=${TAG} -s -w" -o bin/local-path-provisioner-arm64 .
docker buildx build --platform linux/amd64,linux/arm64 \
  -f package/Dockerfile -t "$IMAGE" --push .
```

Use a unique source/build-specific tag, never `latest`. Record the registry index
digest and use `ghcr.io/slaclab/local-path-provisioner@sha256:...` for infrastructure
testing. For an uncommitted build, retain a source snapshot and content fingerprint
alongside the image metadata; the branch HEAD alone does not identify its code.
The runtime image is upstream's scratch image with the static binary and CA
certificates, not a Go toolchain or shell.

## Infrastructure handoff

The supplied migration runbook and older localdev notes describe obsolete
StorageClass-annotation or per-claim-directory behavior. They are not execution
instructions for this image. Before rollout, the runbook owner must update its
configuration table, values, Phase D/Phase H readbacks, and troubleshooting to use
ConfigMap `nodeAffinityKey`; preserve the PV ownership annotation in recreation;
and record the actual outgoing deployed image digest. A local image tag is not
evidence of the deployed version.

Infrastructure testing remains separate: seed the share before provisioning,
confirm two claims see that same file, delete a claim and observe PV removal with
the file intact and zero delete helpers, exercise label loss and missing-directory
failures, and change the ConfigMap key to confirm only new PVs change. Existing-PV
migration, fleet rollout, and rollback gates are not closed by this image build.
