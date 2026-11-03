# E1 Work status compatibility patch

Base: upstream v1.19.0 (6d7b233a54d59c8473803901768153f6e4353d02).
This branch is separate from the standalone PV controller on main.

WorkStatusController previously replaced manifestStatuses with a singleton.
The custom PV migration controller emits paired PV/PVC Works. Keep peer statuses
when one ordinal updates, without mutating the original slice. Conflict retries
and PV/PVC binding validation remain unchanged.

Tests: `go test -mod=vendor ./pkg/controllers/status`.
Build: `buildah bud -f Containerfile.e1 -t docker.io/jeongseungjun/karmada-controller-manager:v1.19.0-e1.1 .`
Push: `buildah push docker.io/jeongseungjun/karmada-controller-manager:v1.19.0-e1.1 docker://docker.io/jeongseungjun/karmada-controller-manager:v1.19.0-e1.1`

Replace ONLY the karmada-controller-manager container image in the existing MGMT
deployment; preserve command arguments, mounts and disabled legacy PV controllers.
See the System repository docs/e1-work-status-v1.19.0.md on branch
restore-automation-20260928 for deployment, rollback and experiment instructions.

Local tests do not substitute for a real two-rank GPU restore evaluation. This
patch is not a claim of general multi-manifest or Pull-mode support.
