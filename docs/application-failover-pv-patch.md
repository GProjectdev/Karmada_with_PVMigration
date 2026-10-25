# Application failover PV preparation patch

## Scope and stop condition

This is a PV preparation patch, NOT a complete StatefulSet-native PVC creation
or end-to-end recovery patch. Preserve the current checkpoint, RestoreRequest,
RestorePlan, PVMigration, Works, PVs and PVCs. Do not remove finalizers or clear
ResourceBinding suspension to make this patch pass.

Changes:

- Reflect actual member PersistentVolume phase into Work manifestStatuses.
- Permit a selected target only under an exact UID-bound restore dispatch hold.
- Grant the PV management controller read access to RestoreRequests.

The selected-target exception requires matching RestoreRequest UID, operation
ResourceBinding UID, workload UID, source/target route, PVMigration name/UID,
sourceFenced and dispatch suspension. It does not adopt legacy holds, create
PVCs, change placement, or release dispatch.

The reported live binding has `stwmig.karmada.io/pv-migration=processing` but
lacks `migration.dcnlab.com/group-volume-hold` and migration receipt links.
It MUST remain blocked after this patch. Do not manufacture those annotations:
the controller owning that existing suspension must first be identified.

## 1. MGMT: preserve evidence

Run in the shell where the three kubeconfig variables are already set.

```bash
set -euo pipefail
: "${KARMADA_KUBECONFIG:?}" "${AWS_KUBECONFIG:?}"
export NS=fluidcr-realign-121040
export BACKUP="/root/gpu-fluidcr-demo/pv-patch-$(date -u +%Y%m%d%H%M%S)"
mkdir -p "$BACKUP"
chmod 700 "$BACKUP"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" \
  get resourcebindings,restorerequests,restoreplans,pvmigrations,fluidcrmigrations \
  -o yaml > "$BACKUP/operations.yaml"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n karmada-es-aws \
  get works -o yaml > "$BACKUP/works.yaml"
kubectl --kubeconfig="$AWS_KUBECONFIG" get pv -o yaml > "$BACKUP/aws-pv.yaml"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" \
  get statefulsets,pods,pvc -o yaml > "$BACKUP/aws-workload.yaml"
kubectl --context=kubernetes -n pv-migration-system \
  get deployment pv-migration-management -o yaml > "$BACKUP/deployment.yaml"
kubectl --context=kubernetes -n pv-migration-system \
  get deployment pv-migration-management \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].image}' \
  > "$BACKUP/old-image.txt"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" \
  get resourceinterpretercustomizations -o yaml > "$BACKUP/interpreters.yaml"
```

## 2. Update source and build with Buildah

Use the checkout containing this document, `go.mod`, `Dockerfile`, and
`config/karmada`. In the demonstrated MGMT layout this is normally
`/root/hybridspot-validation/pv`; confirm before building. Do not reset a dirty
checkout or rebuild unrelated System/FluidCR images for this PV-only patch.

```bash
cd /root/hybridspot-validation/pv
git status --short
git pull --ff-only
test -f docs/application-failover-pv-patch.md
test -f config/karmada/ric/persistentvolume_resource_interpreter.yaml
export PV_PATCH_TAG="pv-failover-$(git rev-parse --short HEAD)-$(date -u +%Y%m%d%H%M%S)"
export PV_PATCH_IMAGE="docker.io/jeongseungjun/multicluster-pvmigration-system:$PV_PATCH_TAG"
buildah bud --layers -f Dockerfile -t "$PV_PATCH_IMAGE" .
buildah push "$PV_PATCH_IMAGE" "docker://$PV_PATCH_IMAGE"
printf '%s\n' "$PV_PATCH_IMAGE" > "$BACKUP/new-image.txt"
```

Stop if checkout updates or either Buildah command fails. Registry login, when
needed: `buildah login docker.io`. Do not put credentials in the guide or Git.

## 3. Check for duplicate PersistentVolume interpreters

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" \
  get resourceinterpretercustomizations -o json |
jq '.items[] | select(.spec.target.apiVersion=="v1" and
  .spec.target.kind=="PersistentVolume") |
  {name:.metadata.name,target:.spec.target,customizations:.spec.customizations}'
```

If an existing interpreter such as `persistentvolume-status` targets PVs,
STOP before applying a second one. Keep its backup and merge statusReflection
into the existing resource; do not delete other customizations. The following
apply assumes there is no conflicting PV interpreter, or the sole interpreter
is already named `persistentvolume` and has the same intended scope.

## 4. Apply Karmada configuration, then replace the MGMT image

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply \
  -f config/karmada/ric/persistentvolume_resource_interpreter.yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply \
  -f config/karmada/rbac/cluster_role.yaml
kubectl --context=kubernetes -n pv-migration-system \
  set image deployment/pv-migration-management manager="$PV_PATCH_IMAGE"
kubectl --context=kubernetes -n pv-migration-system \
  rollout status deployment/pv-migration-management --timeout=180s
kubectl --context=kubernetes -n pv-migration-system \
  logs deployment/pv-migration-management -c manager --tail=100
```

This ClusterRole must be bound to the identity used by the controller's mounted
Karmada kubeconfig. An existing installation with different RBAC names needs its
binding checked; do not assume applying a ClusterRole alone grants access.

## 5. Inspect, do not force progress

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n karmada-es-aws get works \
  pvm-88c83b688b1b642fba18323e pvm-7a35135a65162379d4001fe7 -o json |
jq '.items[] | {name:.metadata.name,conditions:.status.conditions,
  manifestStatuses:.status.manifestStatuses}'
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" \
  get resourcebindings,pvmigrations,restorerequests,restoreplans -o json |
jq '.items[] | {kind,name:.metadata.name,uid:.metadata.uid,
  annotations:.metadata.annotations,spec,status}'
```

Expected evidence for PV reflection: the matching PersistentVolume identifier
and an actual Available/Bound phase, not only Work Applied=True. Missing
manifestStatuses is still a blocker. Inspect Karmada work-status controller
logs and interpreter selection; never synthesize a status receipt manually.

For the current legacy-held operation, a missing exact-operation-hold error is
expected. Save that result. Rollout success is NOT restore success.

## StatefulSet volumeClaimTemplates: remaining integration

System currently calls `EnsureExistingTargetClaims` before releasing placement.
That creates claims from the template using the exact migrated PV, but is NOT
creation by the StatefulSet controller. This patch does not change that path.

To implement native StatefulSet claim creation safely, a separate integration
must stage the target StatefulSet after PV preparation, bind an explicit
RestorePlan name/UID to its Pod template, verify fail-closed member admission,
and allow PVC creation while denying Pod startup until restore preparation.
Tests must cover missing/recreated plans, missing webhooks, wrong volume
bindings, concurrent reconciles, and rollback. Do not claim this integration
or the running cluster E2E recovery has been verified by the PV unit tests.

Before continuing the current operation, collect the current binding's
managedFields and all admission webhook configurations on Karmada to identify
who owns the legacy suspension. Do not guess ownership from annotation names.

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" \
  get resourcebindings -o yaml --show-managed-fields > "$BACKUP/bindings-managed.yaml"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" \
  get mutatingwebhookconfigurations,validatingwebhookconfigurations \
  -o yaml > "$BACKUP/karmada-admission.yaml"
```

## Rollback

```bash
OLD_IMAGE=$(cat "$BACKUP/old-image.txt")
test -n "$OLD_IMAGE"
kubectl --context=kubernetes -n pv-migration-system \
  set image deployment/pv-migration-management manager="$OLD_IMAGE"
kubectl --context=kubernetes -n pv-migration-system \
  rollout status deployment/pv-migration-management --timeout=180s
```

Do not roll back live resources by blindly applying the evidence snapshots.
This rollback does not undo storage operations. The PV interpreter and added
read-only permission can remain; neither releases workload dispatch.
