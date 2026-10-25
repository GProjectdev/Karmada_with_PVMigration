# PV Work status routing repair

Karmada v1.19.0 work status collection requires the member object's annotations
`work.karmada.io/namespace` and `work.karmada.io/name`.
Without the namespace, generateKey ignores the object; without either annotation,
syncWorkStatus cannot route the observation to its Work.

Source:
https://github.com/karmada-io/karmada/blob/v1.19.0/pkg/controllers/status/work_status_controller.go

## Safety and scope

- New Works carry the exact Work name/namespace in their PV manifests.
- Existing generation-1 Works are repaired only after owner labels and the
  entire storage spec match the recorded plan. Foreign/partial routing is rejected.
- A UID-bound generation-2 repair intent is stored in PVMigration status BEFORE
  the optimistic-lock metadata-only Work patch. Recreated or otherwise modified
  Works are not accepted. Preexisting reflected status prevents automatic repair.
- Existing plan hashes remain unchanged. Applied/Available evidence is not forged.
- Cleanup accepts the recorded repaired generation only with matching UID, plan,
  full Work contents and actual reflected PV status. Retain/preservation remain.
- No PV/PVC is deleted or recreated by the repair. Normal verified cleanup still
  detaches Work objects with preserveResourcesOnDeletion=true.
- Existing recover-selected-placement opt-in can resume restoration automatically.
- This does not fix the separate legacy pv-work-* producer or its path conflicts.
  Do not delete that producer's resources without identifying ownership.

## MGMT deployment

Use the root shell containing KARMADA_KUBECONFIG and AWS_KUBECONFIG.
Keep the existing persistentvolume-status interpreter; do NOT create another RIC.
Stop on errors. Do not discard local Git changes.

```bash
(
  set -euo pipefail
  : "${KARMADA_KUBECONFIG:?}"
  cd /root/hybridspot-validation/pv
  git status --short
  git pull --ff-only
  test -f internal/management/work_routing.go
  BACKUP="/root/gpu-fluidcr-demo/work-routing-$(date -u +%Y%m%d%H%M%S)"
  mkdir -p "$BACKUP"
  chmod 700 "$BACKUP"
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n fluidcr-realign-121040 \
    get pvmigrations,restorerequests,restoreplans -o yaml > "$BACKUP/operations.yaml"
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n karmada-es-aws get works \
    pvm-88c83b688b1b642fba18323e pvm-7a35135a65162379d4001fe7 \
    -o yaml > "$BACKUP/works.yaml"
  kubectl --context=kubernetes -n pv-migration-system \
    get deployment pv-migration-management -o yaml > "$BACKUP/deployment.yaml"
  IMAGE="docker.io/jeongseungjun/multicluster-pvmigration-system:work-routing-$(git rev-parse --short HEAD)"
  buildah bud --layers -f Dockerfile -t "$IMAGE" .
  buildah push "$IMAGE" "docker://$IMAGE"
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply \
    -f config/crd/bases/migration.dcnlab.com_pvmigrations.yaml
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply \
    -f config/karmada/rbac/cluster_role.yaml
  kubectl --context=kubernetes -n pv-migration-system \
    set image deployment/pv-migration-management manager="$IMAGE"
  kubectl --context=kubernetes -n pv-migration-system \
    rollout status deployment/pv-migration-management --timeout=180s
)
```

The ClusterRole must be bound to the identity in the controller's Karmada
kubeconfig. If Work patch is forbidden, inspect that binding; do not grant
cluster-admin as a workaround. Missing CRD fields prevent the repair from
advancing, rather than silently allowing an unrecorded edit.

## Verification

```bash
kubectl --context=kubernetes -n pv-migration-system \
  logs deployment/pv-migration-management -c manager --since=5m --tail=80
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n fluidcr-realign-121040 \
  get pvmigrations,restorerequests,restoreplans -o json |
  jq '.items[] | {kind,name:.metadata.name,status}'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" get pv \
  pvm-88c83b688b1b642fba18323e pvm-7a35135a65162379d4001fe7 -o json |
  jq '.items[] | {name:.metadata.name,annotations:.metadata.annotations,status}'
kubectl --kubeconfig="$AWS_KUBECONFIG" -n fluidcr-realign-121040 get pods,pvc
```

Expected: routing annotations appear on AWS PVs, Work reflection becomes
Available/Bound, then PVMigration becomes Ready/Completed. Works can disappear
after normal verified detach; retained PVs must remain. Restore completion still
requires target runtime evidence and resumed training logs, not rollout success.

Do not roll back to an old controller after a generation-2 repair: it rejects
that generation. Preserve the additive CRD status fields and collect logs if
blocked. Never apply backup snapshots wholesale or clear dispatch manually.
