# v6.0 architecture release

PV Migration Controller remains a storage support component with management
and member processes. It is distinct from Multi-Cluster Restore Controller.

Release overlays expose presentation names through annotations. Existing names,
selectors, RBAC, CRDs, mode flags, leader-election identities and reconciliation
behavior are preserved.

Run from this repository on a Linux Buildah host:

```bash
bash scripts/build-v6.0.sh
kubectl kustomize release/v6.0/management > /tmp/multicluster-pvmigration-system-management-v6.0.yaml
kubectl kustomize release/v6.0/member > /tmp/multicluster-pvmigration-system-member-v6.0.yaml
```

Local image: `docker.io/jeongseungjun/multicluster-pvmigration-system:v6.0`.
Build scripts do not push or deploy. Review and commit source before producing
publishable images so revision labels identify reviewed content. These files
do not imply that v6.0 tags have been published. Publication and cluster changes
belong to a separate release step: record digests and back up live manifests
first, retain existing secrets/certificates/kubeconfigs/storage, and use the
prior manifests and image digests for rollback.
