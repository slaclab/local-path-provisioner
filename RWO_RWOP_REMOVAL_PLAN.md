# Plan: remove RWO and RWOP from the S3DF LPP v2 image (shared mode)

The trial patch works. In the trial worktree, `go test -race -tags=helm .` passes 30 `TestS3DF*` top-level tests and 79 `TestS3DF*` sub-tests (up from 29 and 71), with 0 failures. I recommend doing this in shared mode only and leaving node-local mode as it is, but that is your call (section 0).

- **Repo:** `/Users/amithm/Documents/projects/lpp/lpp-s3df-image`, branch `s3df-image-v2-20260918` @ `4e564dc6`, working tree clean.
- **Trial worktree:** `/Users/amithm/Documents/projects/lpp/local-path-provisioner/.claude/worktrees/wf_180968b7-517-1`, detached at `4e564dc6`. It has 8 uncommitted files: `README.md`, `S3DF_IMAGE.md`, `deploy/chart/local-path-provisioner/values.yaml`, `provisioner.go`, and the 4 test files `provisioner_{affinity,config,s3df,sharedroot}_test.go`. I re-checked this list today.
- Nothing was committed, pushed or built, and no cluster was touched. The two read-only repos were not edited.
- Line numbers refer to `4e564dc6` unless stated otherwise. After the patch, any `provisioner.go` line past `:385` moves down 12 lines.

## 0. Scope decision: is node-local mode used?

- **How the mode is chosen.** The mode comes from the config. A non-empty `nodePathMap` means node-local mode, and `sharedFileSystemPath` alone means shared mode. Setting both is an error (`provisioner.go:267-285`, `:273`, `:277`). The choice is made per StorageClass config (`pickConfig`, `:287-300`).
- **Where access modes are checked.** Only `if !sharedFS` (`provisioner.go:374-386`).
  - Node-local accepts only RWO/RWOP (`:378-381`). It also accepts an empty access-mode list, because the loop never runs.
  - Shared mode checks no access modes at all. It also skips the `Selector` rejection (`:375-377`).
- **The current shared test** provisions all four modes and expects 4 helpers (`provisioner_sharedroot_test.go:19`, `:33`).
- **Legacy behaviour.** The legacy image accepts RWX and ROX and rejects RWO, but this is based on watching it run, not on its source code. The running binary does not match its source (`LPP-V2-INSTANCE-REFERENCE.md:189`, `:194`, `:585`).
  - Legacy behaviour for RWOP and for mixed lists is not recorded anywhere.
  - The message "Only support ReadWriteMany or ReadOnlyMany access mode" appears in no file in either repo (grep checked).
  - The sage-dev rejection is recorded only as "neither RWX nor ROX" (`LPP-STORAGECLASS-WORKLOAD-ANALYSIS.md:145`).
- **P10 status.** Blocking RWO/RWOP is still an open decision: P10 (`LPP-UPGRADE-CHANGE-PLAN.md:267`), L8 (`LPP-V2-INSTANCE-REFERENCE.md:576`), D10 (`:359`) and Untested 15 (`LPP-UPGRADE-CHANGE-PLAN.md:510`).
- **No v2 deployment is planned to use node-local mode.**
  - P3 and the B3 read-back require `sharedFileSystemPath` and `nodeAffinityKey`, with no `nodePathMap` (`LPP-UPGRADE-CHANGE-PLAN.md:259`, `:303`).
  - Keeping `nodePathMap` under v2 is listed as a rejected alternative (`LPP-V2-INSTANCE-REFERENCE.md:595`).
  - The image contract's instance config uses `nodePathMap: []` (`S3DF_IMAGE.md:33-42`).
- **Every testbed v2 config is shared mode.**
  - Configs: `dev/lpp-validation/instance/configmap-patch.yaml:9`, `active-upgrade/manifests/host/v2-configmap.yaml:9-11`.
  - Enforced by `assertions.jq:17` and `claim-recreation/check-manifests.sh:74`.
  - `nodePathMap` appears only in the legacy rollback config (`active-upgrade/manifests/host/legacy-configmap.yaml:9`, `prepare-instance.sh:38-43`).
  - Every testbed claim is RWX (`templates/pvc.json:6`, `assertions.jq:57`, `check-manifests.sh:30`).
- **Where `nodePathMap` does appear.**
  - The 40 `sdf-k8s01/*/*-values.yaml` files, for example `sdf-k8s01/common/sdf-home-values.yaml:1-2`. These run the legacy image (`sdf-k8s01/kustomization.yaml:27-30`).
  - The chart default, which is node-local (`deploy/chart/local-path-provisioner/values.yaml:88-91`).
  - Upstream e2e tests and examples. The only shared e2e fixture is `test/testdata/multiple-storage-classes`; it uses an RWX claim on the shared class (`pvc.yaml:18-20`), and its RWO claim is on the node-local class.
- **No PV in the fleet is RWO or RWOP.**
  - The 2026-09-22 dump (`active-upgrade/artifacts/upgrade-20260922t225558z/streams/host-pvs-list.json`) has 521 LPP PVs: 512 production and 9 testbed. They split 400 RWX and 121 ROX, all `Directory`, with no hostname affinity.
  - The dump also has 5 RWO hostPath PVs, but they are not LPP: 3 vault PVs and 2 `vcluster-log-pv-*` PVs.
  - This loosely matches your 2026-09-30 figure of 516 = 394/122/0.
- **GitOps tenants.** Across alpastor, only one manifest asks for RWO on an LPP class, and it is a test class: `local-path-provisioner/dev/lpp-upgrade/tests/approach3-newsc/pvc-pod.yaml:11-12`. vCluster etcd claims use wekafs or the default class, and LPP classes are not the default (`sdf-k8s01/rubin/sdf-data-rubin-helm.yaml:76`).
- **Existing hazard (C9).** If a legacy `nodePathMap` ConfigMap reached v2, RWX and ROX claims would fail. An RWO claim would get a `DirectoryOrCreate` folder that can land on local disk (`provisioner.go:658`, `:969`; `LPP-V2-INSTANCE-REFERENCE.md:210-213`, `:555`). Today only procedure prevents this (`LPP-UPGRADE-CHANGE-PLAN.md:169-175`).

**Recommendation: Option A.** Shared mode accepts RWX and ROX only. Node-local mode stays unchanged.

- **Option B** (remove RWO/RWOP from node-local too) would leave node-local accepting no access mode at all. The mode would be disabled silently, one claim error at a time.
- **Option C** (a separate change): reject a non-empty `nodePathMap` when the config loads. This also closes the C9 hazard. It costs upstream parity and the chart default, and it changes edge case 3 (`LPP-STORAGECLASS-WORKLOAD-ANALYSIS.md:221`) and `LPP-UPGRADE-CHANGE-PLAN.md:170`.
- **Admission policy alternative.** P10 offers either "a v2 change or an admission policy" (`:267`). Kyverno already enforces per-vCluster PVC rules, for example `alpastor/kyverno/sdf-k8s01/policies/vclusters/enforce-k-limit-storageclass-k-psdm-dev.yaml:1-31`.
  - A policy rejects the claim when it is created, with no retries or event noise, and works on legacy immediately.
  - The image change works even without the policy and keeps the PV from advertising RWO on a whole share.
  - Doing both is reasonable. The image change alone is enough.

## 1. Code change (trial diff, `provisioner.go`)

**Where it goes.** Add an `else` branch to the existing `if !sharedFS` block, right after `:385`.
- It runs before `getPathOnNode` (`:398-401`), the pattern rejection (`:406-413`) and the helper pod creation (`:503-514`). Nothing between `:386` and `:503` calls the API.
- `pickConfig` and `isSharedFilesystem` errors still come first (`:359-361`, `:370-372`).

```go
	} else {
		// Shared facility roots are multi-node by nature; single-node modes
		// (ReadWriteOnce, ReadWriteOncePod) are not supported, matching the
		// legacy S3DF image.
		if len(pvc.Spec.AccessModes) == 0 {
			return nil, pvController.ProvisioningFinished, fmt.Errorf("sharedFileSystemPath only supports ReadWriteMany and ReadOnlyMany access modes: no access mode requested")
		}
		for _, accessMode := range pvc.Spec.AccessModes {
			if accessMode != v1.ReadWriteMany && accessMode != v1.ReadOnlyMany {
				return nil, pvController.ProvisioningFinished, fmt.Errorf("sharedFileSystemPath only supports ReadWriteMany and ReadOnlyMany access modes: %s is not supported", accessMode)
			}
		}
	}
```

**Why `ProvisioningFinished`.**
- Every existing rejection already returns it (`provisioner.go:372`, `:376`, `:380`, `:411`).
- It drops the claim from `claimsInProgress` and requeues it (vendored `controller.go:1096-1111`), and it posts a Warning `ProvisioningFailed` event (`:1524`).
- `ProvisioningInBackground` would keep a stored copy of the claim and keep retrying after the claim is deleted, until the retry threshold (`:1113-1116`, `:1056-1061`, `:971-973`).
- `IgnoredError` posts no event (`:1490-1493`).

**What tenants see.** The controller adds a prefix to the error: `failed to provision volume with StorageClass "<class>": sharedFileSystemPath only supports…` (`controller.go:1497`). Each attempt also posts a Normal `Provisioning` event first (`:1486`).

**Optional message change.** `sharedFileSystemPath` is an internal ConfigMap key that tenants never see. A tenant-facing phrasing would be clearer, for example "this StorageClass is a shared facility root; only ReadWriteMany/ReadOnlyMany are supported (got ReadWriteOnce)". If you change the message, update the test strings to match.

**Mixed and empty lists.**
- A claim is rejected if any listed mode is RWO or RWOP, so `[RWX,RWO]` and `[ROX,RWOP]` are rejected and `[RWX,ROX]` is accepted.
- Mixed lists have to be rejected, because the PV copies the claim's modes unchanged (`provisioner.go:589`). Without the rejection it would advertise RWO on the whole share.
- The empty-list check is just a cheap guard. Kubernetes API validation should already block empty lists, but that code is not vendored, so this is unverified. Node-local still accepts an empty list; this difference is harmless.

**Ordering change.** An RWO claim on a class with `pathPattern` now gets the access-mode error instead of the pattern error from `:411`.

## 2. Node-local mode

Unchanged: `provisioner.go:374-386` is byte-identical in the trial. Shared mode still accepts `pvc.Spec.Selector`; that was already the case and is out of scope, so note it but don't change it. Under Option C, the change would go in `isSharedFilesystem` (`:267-285`).

## 3. Tests (trial edits)

**With only the code change, 15 top-level tests fail.** Two verifiers reproduced this, and the trial's count of 11 was wrong. The extra 4 are:
- `TestS3DFAffinitySource`
- `TestS3DFSharedPathOptionsRejected`: the access-mode error fires before `ErrorContains(err, key)` at `provisioner_sharedroot_test.go:113`.
- `TestS3DFSharedHelperMountOverrideRejected`: it expects "shared filesystem" at `:147`.
- The helm-only `TestS3DFChartConfigMapKey` (`chart_s3df_test.go:80-84`).

The cause is that the fixture defaults to RWO (`provisioner_s3df_test.go:71`).

The test edits:

1. **Fixture** (`provisioner_s3df_test.go:71`): change the default to `{v1.ReadWriteMany}`. Add `s3dfNodeLocalTestOptions()`, which returns the same fixture with `{v1.ReadWriteOnce}`.
2. **Node-local tests** switch to the new helper:
   - `TestS3DFNodeLocalSubdirectories` (`provisioner_sharedroot_test.go:61`)
   - `TestS3DFNodeLocalOwnershipPatterns` (`provisioner_config_test.go:127`)
   - `TestS3DFAffinitySource` when `tt.node != nil`, after `provisioner_affinity_test.go:82`. Only the 4 `local *` cases set `node` (`:56-77`).
3. **`TestS3DFSharedRoot`** (`:19`, `:33`): loop over `{ROX, RWX}` and expect 2 helpers.
4. **New `TestS3DFSharedAccessModesRejected`** (adds the `pvController` import):

```go
for _, tc := range []struct{ name string; modes []v1.PersistentVolumeAccessMode; ok bool }{
  {"rwo", {RWO}, false}, {"rwop", {RWOP}, false}, {"rwx+rwo", {RWX, RWO}, false},
  {"rox+rwop", {ROX, RWOP}, false}, {"empty", nil, false},
  {"rwx", {RWX}, true}, {"rox", {ROX}, true}, {"rwx+rox", {RWX, ROX}, true},
} {
  opts := s3dfTestOptions(); opts.PVC.Spec.AccessModes = tc.modes
  pv, state, err := p.Provision(t.Context(), opts)
  if !tc.ok { require.ErrorContains(err, "only supports ReadWriteMany and ReadOnlyMany"); require.Nil(pv)
              require.Equal(pvController.ProvisioningFinished, state); require.Empty(helpers) }
  else      { require.NoError(err); require.Equal(tc.modes, pv.Spec.AccessModes)
              require.Equal(shareRoot, pv.Spec.HostPath.Path); require.Len(helpers, 1) }
}
```

5. **To add (gaps the verifiers found):**
   - A node-local case that pins "unchanged": RWX rejected with "NodePath only supports", and RWOP accepted. No `*_test.go` covers either today.
   - An ordering case: RWO plus `pathPattern` gives the access-mode error.
   - A shared case configured per class through `storageClassConfigs`.
   - Optionally, a controller-level check of the `ProvisioningFailed` event, reusing the harness in `TestS3DFControllerReclaimsReleasedPV` (`provisioner_s3df_test.go:167`).

**Results** (re-run in the trial worktree):
- `go test -race -tags=helm .` passes: 29 → 30 top-level and 71 → 79 sub-tests for `TestS3DF*`, or 103 → 111 sub-tests across the whole package.
- Without `-tags=helm`, 25 `TestS3DF*` tests run and pass; the other 5 are in `chart_s3df_test.go`, which is helm-tagged. CI must pass `-tags=helm`.
- `go vet` and `gofmt` are clean, and the e2e suite compiles.

I dropped the draft's "loose assertion near `:130`" item. That line is in `TestS3DFInvalidSharedRootRejected`, which never calls `Provision`.

## 4. Docs

**This repo.** The trial already edited the first three items.
- `S3DF_IMAGE.md:66` (H3) currently says "RWX/ROX/RWO remain supported", which also leaves out RWOP. The trial's new text says only RWX and ROX are supported.
- `README.md:207` and the comment at `values.yaml:94-95`.
- Add an access-mode row to the requirements table at `S3DF_IMAGE.md:9-18`. Make this required, not optional.
- `templates/NOTES.txt:8-9`: make the example claim depend on the mode or make it neutral. RWO is still correct for the node-local chart default. This is cosmetic, since GitOps uses `helm template`.
- Stale RWO-on-shared files:
  - `test/dev-e2e-tests.sh:308-395` (Test 3)
  - `test/dev-e2e-examples.yaml:135-157`
  - `test/dev-e2e-README.md:12`, `:162`, `:171`
  - `localdev/README.md:22`, `:145-160`
  - `localdev/tests/r3-access-modes.yaml:2-14`, `:60-69`. It has an RWO claim only; RWOP appears only in comments. It also cites a stale line, `provisioner.go:354`.
  - `LOCAL_DEV_TESTING.md:43`, `:122`, `:137`, `:155`, `:210-211`
- `S3DF_IMAGE.md:118-120` already marks the localdev notes as non-authoritative.
- Leave these alone:
  - `UPLIFT_KIND_TEST_REPORT.md:212`, `:327`, which describe node-local mode
  - `README.md:84`, `:91`, the node-local quickstart
  - `feature-taks..xml:18`, a JIRA export
  - `S3DF_IMAGE.md:84` (`-run '^$'`)

**alpastor follow-ups (not edited):**
- `LPP-UPGRADE-CHANGE-PLAN.md`:
  - P10 (`:267`): mark it DECIDED, with the new digest and commit.
  - Item 4 (`:180-181`): change to "v2 rejects them, as legacy does".
  - P5a (`:261`): replace "RWO claims, or their block (P10)" with the cases in section 5.
  - Untested 15 (`:510`).
  - Rollback condition 2 (`:422`): limit it to claims whose modes are all RWX and/or ROX. A mixed `[RWX,RWO]` claim is now rejected on purpose and would otherwise trigger a rollback.
  - Checks and balances (`:405`): add that pods Pending on a rejected claim are expected.
  - The Phase 1 gate (`:92`): re-check it once P10 is closed.
- `LPP-V2-INSTANCE-REFERENCE.md`:
  - The v2 cell of the behaviour table (`:189`): "RWX, ROX (RWO/RWOP rejected before any helper)". The upstream column stays "All".
  - Replace D10 (`:359`) with a new D12, "Shared mode accepts RWX and ROX only".
  - Row `:559`: change it to "Rejected: claim Pending, ProvisioningFailed event, no helper, no PV". Add rows for ROX and for mixed lists.
  - Close L8 (`:576`).
  - D1 "Why" (`:208-209`): add "and rejects RWO/RWOP".
  - "Not tested" (`:631`).
  - Test counts at `:370`, `:584`, `:605`, and add the new test name to the list at `:606-616`.
- `LPP-STORAGECLASS-WORKLOAD-ANALYSIS.md:233` (edge case 15).
- `alpastor/.github/skills/lpp-s3df-active-upgrade/SKILL.md:140-141` (it says "accepts RWO/RWOP").
- Wording: where only "non-RWX/ROX" was observed (`:189`, analysis `:233`, `SKILL.md:141`), don't call the sage-dev claim RWO.

## 5. Operational impact

- **New RWO/RWOP claims stay Pending,** as they do under legacy for RWO.
  - Each attempt posts a Normal `Provisioning` event and a Warning `ProvisioningFailed` event.
  - Host events sync into vClusters (`alpastor/vclusters/values/0.31/vcluster-values.yaml.tmpl:120-124`), so tenants like sage-dev will see the reason.
  - Under WaitForFirstConsumer, the consumer pod stays pinned to its selected node while Pending (`controller.go:1525`, `:1548-1550`).
- **Retry pattern** (my reading of the code):
  - There are up to 16 attempts, because the check is `NumRequeues < 15` (`controller.go:200`, `:967-971`). Exponential back-off runs from 15 s up to a 1000 s cap (`:194`, `:684-688`), so this burst lasts about 2.75 hours.
  - After that, the 15-minute resync (`:190`, `:713`) triggers one more attempt each time, indefinitely, because the requeue count is not cleared (`:974-975`).
  - Identical events are merged into one Event object with a rising count (`client-go/tools/record/events_cache.go:323-343`). Measure `.count` and `lastTimestamp`, not the number of Event objects.
- **Existing PVs are unaffected.**
  - `Delete` ignores access modes (`provisioner.go:600-619`).
  - P4 recreates PVs without calling Provision (`LPP-UPGRADE-CHANGE-PLAN.md:260`).
  - There are 0 RWO LPP PVs (section 0).
  - Claims that tenants re-create, including StatefulSet template claims, do go through Provision (Untested 17, `:508`). They are safe only because every existing claim is RWX or ROX.
- **ROX is unchanged by this patch, but it has never been run live on v2** (`:189`, Untested 5 at `:500`). ROX does not give write protection on hostPath; 51 mounts are writable (`LPP-STORAGECLASS-WORKLOAD-ANALYSIS.md:203`).
- **Rollback.** Rolling back to legacy is the same as v2 for RWO, as observed. For RWOP and mixed lists, legacy behaviour is unknown.
- **Rollout order:**
  1. Commit and build (section 6).
  2. Use the stop-first switch to move the testbed from the current v2 to the new v2.
  3. Run P5a on the new digest (`:261`; P5a has not run yet).
  4. Close P10.
  5. Phase 1 (`:92`).

  Production never runs a digest that accepts RWO.
- **New P5a cases.** Each needs a consumer pod, because the class is WaitForFirstConsumer (`LPP-V2-INSTANCE-REFERENCE.md:467`).
  - An RWO claim made through a vCluster, and an RWOP claim. Each stays Pending, gets the prefixed message, and creates no helper and no PV.
  - A ROX claim with a read-only consumer binds to the root with `Directory`.
  - `[RWX,ROX]` binds; `[RWX,RWO]` and `[ROX,RWOP]` are rejected.
  - Event count over at least 15 minutes.
  - An RWO claim created during the stop-first pause.
  - During the rollback rehearsal, record legacy's actual rejection text and event count for RWO and RWOP. Don't assert a message nobody has captured.
  - The 12 testbed PVs are unchanged across the switch.
  - Add `.spec.accessModes` to the A1 query of Pending claims, so existing Pending RWO claims are recorded before the switch.
- **Testbed harness changes** (`dev/lpp-validation`):
  - Make the access mode a parameter in `create_claim` (`lifecycle.sh:3-9`, `templates/pvc.json:6`).
  - Make `pv()` at `assertions.jq:57` compare against the claim's own modes.
  - Widen the claim-name regex (`assertions.jq:166`).
  - Add a consumer pod that doesn't wait for Ready; `create_pod` does (`lib.sh:363`).
  - Add rejection checks: event on the claim's UID, claim Pending, no PV.
  - Clean up inside the run, because `cleanup.sh:26-27` refuses while any claim remains.
  - Add self-test cases (`self-test.sh:150-163`).
  - `validate_trace` already catches a stray helper (`capture.sh:123-126`).

## 6. Image build and identity

- **Provenance gap.** Commit `4e564dc6` is dated 2026-09-18 (`git log`). The image research found that the deployed digest `ghcr.io/slaclab/lpp:s3df-v2@sha256:c51973c9…` was created on 2026-09-10; I did not re-check that date.
  - The docs already say no build record links the digest to the commit (`LPP-V2-INSTANCE-REFERENCE.md:10`, `:368`).
  - But if the image predates the commit, calling `4e564dc6` its source is misleading.
- **New build:**
  - Commit from a clean tree.
  - Tag `ghcr.io/slaclab/lpp:s3df-v2-<date>-<sha7>`, with `-X main.VERSION` set to the same string (`S3DF_IMAGE.md:101-102`).
  - Add OCI revision and source labels.
  - Record the Go version, the binary sha256, the digest and the build time (`S3DF_IMAGE.md:109-112`).
  - Build both architectures, or amd64 only. `bin/` currently has a stale arm64 binary.
  - Fix the registry name at `S3DF_IMAGE.md:100`, `:110`: it says `ghcr.io/slaclab/local-path-provisioner`, but the deployed repo is `ghcr.io/slaclab/lpp`.
- **Tag constraint.** `prepare-instance.sh:113` checks for the substring `s3df-v2`. Keep that in the tag, or switch to an exact-version check, because the substring also matches the old binary.
- **Digest references to update** (grep-complete):
  - `dev/lpp-validation`: `settings.json:11-12`, `instance/deployment-patch.yaml:18`, `active-upgrade/manifests/host/v2-deployment-patch.yaml:17`, `README.md:53`, `active-upgrade/manifests/README.md:128` (add the new digest to its check), `claim-recreation/README.md:56`, `:97`
  - `LPP-UPGRADE-CHANGE-PLAN.md:31`, `:534`. Keep `c51973c9` as the v2 rollback target.
  - `LPP-V2-INSTANCE-REFERENCE.md:10`. Annotate `:457` as historical.
  - `SKILL.md:58`
- **Commit references to update** (not digests): `LPP-V2-INSTANCE-REFERENCE.md:199`, `:604`, `:637`. Plain `provisioner.go:NNN` citations past `:385` move by 12 lines. Pinned permalinks stay valid, for example `PV-RECREATION-RESEARCH-MEMO.md:462-463`.

## 7. Risks and untested

- Nothing in this change has run live yet: RWO/RWOP rejection, ROX and mixed lists. Every recorded testbed run was RWX on `c51973c9`, and no artifact contains `ProvisioningFailed`.
- "Matches legacy" is proven for RWX, ROX and RWO alone. For RWOP and mixed lists it is assumed, and the legacy message text has no source.
- RWO claims already Pending in production keep failing after B4, as they did under legacy. Record them first with the A1 query, or they may be misread as a v2 regression.
- Event noise: two events per attempt and retries that never stop. Narrow rollback condition 2 and count `.count`, not Event objects.
- The ordering change: a tenant with a mis-built class sees the access-mode error before the `pathPattern` error.
- Option A keeps the C9 hazard, which only procedure prevents.
- The stale pre-v2 scripts in section 4 fail if run unchanged.
- The image's provenance, and whether the new build can be reproduced, are not verified.

## Verification notes

- Trial worktree confirmed today: at `4e564dc6` with exactly the 8 modified files listed above.
- The code-only failure count is 15. Two verifiers reproduced it independently, so the trial's 11 is wrong.
- The sub-test comparison is corrected to 71 → 79 for `TestS3DF*` (103 → 111 for the whole package).
- Dropped the draft's "loose assertion near `:130`" item; all three verifiers refuted it.
- Fleet counts corrected to 512 production plus 9 testbed. The 5 RWO PVs in the dump are not LPP.
- Re-grepped both repos today for "Only support ReadWriteMany": no matches.
- `LPP-V2-INSTANCE-REFERENCE.md` lines `:199`, `:604` and `:637` are commit references, not digest references.
- Not re-run by me: the image creation date, and the Kubernetes API's rejection of empty access-mode lists.
