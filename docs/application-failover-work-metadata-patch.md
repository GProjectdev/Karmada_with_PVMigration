# Work metadata comparison repair

This patch accepts the three Karmada tracking metadata keys added to embedded
PV manifests. It also normalizes an omitted/null creationTimestamp. It does
not ignore PV specs, migration ownership labels, arbitrary metadata, or Work
generation changes. Comparison operates on a copy; existing Works are not edited.

Keep the existing persistentvolume-status interpreter. Do not install a second
PersistentVolume interpreter or delete Works/PVs/PVCs. The existing recovery
opt-in may automatically continue restoration after PV preparation succeeds.

Run on MGMT after confirming the checkout has no conflicting local changes:

```bash
(
  set -euo pipefail
  cd /root/hybridspot-validation/pv
  git status --short
  git pull --ff-only
  test -f internal/management/work_metadata_test.go
  IMAGE="docker.io/jeongseungjun/multicluster-pvmigration-system:work-metadata-$(git rev-parse --short HEAD)"
  buildah bud --layers -f Dockerfile -t "$IMAGE" .
  buildah push "$IMAGE" "docker://$IMAGE"
  kubectl --context=kubernetes -n pv-migration-system \
    set image deployment/pv-migration-management manager="$IMAGE"
  kubectl --context=kubernetes -n pv-migration-system \
    rollout status deployment/pv-migration-management --timeout=180s
)
```

Inspect current evidence:

```bash
kubectl --context=kubernetes -n pv-migration-system \
  logs deployment/pv-migration-management -c manager --since=5m --tail=80
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" \
  -n fluidcr-realign-121040 get pvmigrations,restorerequests,restoreplans -o json |
  jq '.items[] | {kind,name:.metadata.name,status}'
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n karmada-es-aws get works \
  pvm-88c83b688b1b642fba18323e pvm-7a35135a65162379d4001fe7 -o json |
  jq '.items[] | {name:.metadata.name,status}'
```

Work Applied=True alone is insufficient: actual PV Available/Bound reflection
must be present. Missing manifestStatuses remains a separate blocker. Never
fabricate receipts or release dispatch manually. Local unit tests do not prove
live cross-cluster training recovery.
