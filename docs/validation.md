# Validation

Validated on 2026-09-25 with Go 1.27.1 on Windows; the module minimum and CI toolchain are Go 1.24.

## Completed locally

- `go test -mod=readonly -count=1 ./...`: all packages passed.
- `go vet ./...`: passed.
- Linux amd64 build with `CGO_ENABLED=0`: passed.
- Windows manager build and `manager.exe --help`: passed, including `--kubeconfig` registration.
- Kustomize 5.6.0 rendered all five bundles: CRDs, hosted management, member, Karmada RBAC, Karmada RIC.
- CRDs passed Kubernetes structural schema and CEL validation with API defaulting.
- Tests execute the actual YAML-embedded Lua scripts using gopher-lua.

Controller tests cover bound PV/PVC identity, nonzero StatefulSet ordinals, all claim templates, last-good snapshot preservation, unsupported storage rejection, source fencing and dispatch suspension gates, source-fixed metadata propagation, immutable plan hashes, duplicate target requests, stale or foreign Work evidence, and restart-safe Work detachment.

## Not executed locally

No live Karmada/member API connection, Docker daemon, NFS mount, CRIU restoration, or cloud storage operation was used. Cross-compiling a binary and testing fake clients do not prove deployment or data integrity on a real cluster. The Linux CI workflow additionally runs the Go race detector; its result is separate from local verification.

## Cluster acceptance procedure

1. Use disposable source and target clusters with a reachable shared NFS export. Install the CRDs on Karmada and both members, RIC on Karmada, and the appropriate controllers as in README.
2. Deploy a labeled StatefulSet on the source, write a known payload, and record its checksum. Confirm every expected claim-template/ordinal appears in Karmada `PVMetadata.status.clusters[].volumes` with correct source identities.
3. Suspend workload dispatch before changing placement, disable competing failover/resume actors, and verify source writers have stopped. Preserve the source PV and PVC retention policies. An unreachable node requires external fencing, not merely an API request to stop a Pod.
4. Submit an explicit `PVMigration` for an unused target. Confirm source PVs/PVCs stay untouched and only target PV Works are created. Restart the management Deployment during preparation to exercise durable planning.
5. Wait for `status.phase=Completed`. Confirm each recorded Work is gone from Karmada and its PV still exists on the target with `Retain` and the expected namespace/PVC claimRef. The completed CR remains as the reservation/history record.
6. Prepare checkpoint restoration separately, update the workload's actual PropagationPolicy, and release dispatch only after all required gates pass. Verify target PVCs bind to the retained PVs and the payload checksum matches.
7. Reconcile or restart controllers again. Confirm deleted PV Works are not recreated. Submit an overlapping request and confirm it is blocked without an additional PV.

Also exercise source unavailability after a successful snapshot, a missing PVC, a changed NFS path after planning, a CSI/EBS source, and a target already in ResourceBinding placement. These cases must retain the old snapshot or block migration without deleting storage.
