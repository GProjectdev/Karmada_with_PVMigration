# PV Migration System Architecture

## Module and runtime contract

This project is a standalone Go module:

```text
github.com/GProjectdev/Karmada_with_PVMigration
```

The controller binary is expected at `/manager` and supports two modes:

```bash
/manager --mode=management --kubeconfig=/path/to/karmada.config --health-probe-bind-address=:8081 --metrics-bind-address=:8080 --leader-election-namespace=pv-migration-system --poll-interval=15s --leader-elect=true
/manager --mode=member --cluster-name=aws --health-probe-bind-address=:8081 --metrics-bind-address=:8080 --leader-election-namespace=pv-migration-system --poll-interval=15s --leader-elect=true
```

Management mode uses only the Karmada kubeconfig for Kubernetes clients and leader-election leases. The pod itself can run on an ordinary host cluster, but the mounted Secret must contain a flattened Karmada kubeconfig.

Member mode uses the in-cluster ServiceAccount and must be patched with the member cluster name that Karmada uses, for example `aws`.

## API summary

`PVMetadata` is namespaced and records stateful workload volume metadata for one source cluster plus the Karmada-aggregated per-cluster view. `status.volumes[*].pvSpec` is an object with unknown fields preserved so native PersistentVolume spec details survive round trips.

`PVMigration` is an explicit attested request. Its spec is immutable immediately after creation, enforced by a CEL transition rule in the CRD. A request must set `sourceFenced=true`, name the source and target clusters, point to a `PVMetadata`, and explicitly map each source PVC to its target PVC.

`PVMigration.status.planHash` stores the frozen migration plan hash. The controller computes it from the generated Work specs plus source-local identity fields: workload UID, PVC UIDs, and PV names. `collectedAt` is not part of the hash because it changes on polling without changing migration identity. After planning, snapshot identity or PV spec changes must be rejected instead of silently changing the plan.

## Karmada behavior

Management discovers opt-in StatefulSet ResourceBindings whose template has:

```yaml
metadata:
  labels:
    migration.dcnlab.com/pv-metadata: enabled
```

For each workload and cluster, management creates `PVMetadata` and a source-fixed PropagationPolicy so metadata collection itself does not fail over. This policy is only for metadata objects; it does not prevent the real workload ResourceBinding from moving. Operators must disable stock workload failover or scheduler movement before pre-staging target PVs.

Karmada `ResourceInterpreterCustomization` is installed separately. Its `ReflectStatus` returns only local fields, and `AggregateStatus` writes `status.clusters` in deterministic cluster-name order. Aggregation keeps last-good volume records when a new reflected item is missing, stale, or not ready, while marking that cluster not ready instead of falsely reporting stale readiness.

## Migration state machine

The manager default for `--metrics-bind-address` is `0`, so metrics are disabled unless the deployment explicitly sets a bind address such as `:8080`.

`PVMigration.status.phase` uses:

- `Pending`: prerequisites or snapshot validation have not passed; read `status.message`.
- `Preparing`: validation passed and target retained PV Work is being created or observed.
- `Ready`: target PV was identity-checked as the requested retained PV and reflected as `Available` or `Bound`; Work is applied for the current create-once immutable spec. Karmada v1.14 Work `Applied` conditions do not always carry `observedGeneration`, so generation absence or `0` is accepted only for generation `<=1` create-once Work checks.
- `Completed`: cleanup detached the Work after durable status recorded `applied=true`; retained PV remains.
- `Failed`: validation failed or an unrecoverable guardrail blocked the request.

Source snapshot selection can read from `status.clusters` last-good records even when the source is offline or `ready=false`, but only if a usable last-good item includes volume records and a coherent snapshot generation. The RIC keeps last-good `volumes`, `workloadUID`, `collectedAt`, and `observedGeneration` together so old data is not tagged as a newer generation.

## Migration guardrails

Migration is intentionally narrow:

- NFS-only retained PV reconstruction.
- No EBS, CSI, or provider-specific volume handles.
- No member credentials in management mode.
- No data-byte copying.
- No automatic source deletion.
- No automatic ResourceBinding resume.
- Existing target pre-staging is unsupported when the target is already present in the ResourceBinding cluster list.

Before a migration starts, operators must suspend StatefulSet dispatching in the ResourceBinding and attest that the source workload is fenced or stopped externally. A safe manual fence can be scaling the source workload to zero while preserving the last PVMetadata snapshot. Operators must also record the source claim Retain state before migration; this is an external operational requirement, not something the migration controller can infer after the fact.

Completed `PVMigration` objects are migration history and duplicate target PVC reservation records. Controllers should not automatically prune them; deleting history is an operator decision.
