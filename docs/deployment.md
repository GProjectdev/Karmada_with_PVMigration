# Deployment Notes

## Config layout

- `config/crd`: CRDs for `PVMetadata` and `PVMigration`.
- `config/management`: hosted management controller Deployment for any ordinary host cluster.
- `config/member`: member-cluster controller Deployment using in-cluster ServiceAccount credentials.
- `config/karmada/rbac`: Karmada control-plane RBAC for the management controller identity represented in the Karmada kubeconfig.
- `config/karmada/ric`: Karmada ResourceInterpreterCustomization for PVMetadata health, reflection, and aggregation.
- `config/samples`: minimal PVMetadata and PVMigration examples.

## Image and runtime

Build and push the image:

```bash
make docker-build IMG=ghcr.io/gprojectdev/pv-migration-system:dev
make docker-push IMG=ghcr.io/gprojectdev/pv-migration-system:dev
```

Management command:

```bash
/manager --mode=management --kubeconfig=/etc/karmada/kubeconfig --health-probe-bind-address=:8081 --metrics-bind-address=:8080 --leader-election-namespace=pv-migration-system --poll-interval=15s --leader-elect=true
```

Member command:

```bash
/manager --mode=member --cluster-name=aws --health-probe-bind-address=:8081 --metrics-bind-address=:8080 --leader-election-namespace=pv-migration-system --poll-interval=15s --leader-elect=true
```

## Operational stop conditions

A completed migration means the retained PV Work was applied to the target and then detached without deleting the target PV. For Karmada v1.14, Work `Applied` condition generation may be absent or `0`; this is accepted only for generation `<=1` create-once immutable Work with an identity-checked reflected PV. Completion does not mean the application was restored, traffic was moved, data was copied, or the ResourceBinding was resumed.

The `PVMigration` CR should remain after completion. It preserves target PVC reservations and plan history so a later request cannot accidentally reuse the same target PVC without an explicit operator decision.

Resume the workload only after an external checkpoint or restore gate says it is safe.

When a higher-level HybridSpotVM policy flow consumes this status, `Completed` is still
only historical PV evidence. Policy Manager must not delete the old `NodeProvision`
until restore evidence is verified against the current `PVMigration` UID, observed
generation, plan hash, source metadata, target PVC mappings, and restore operation UID.
PV-Migration-System never resumes ResourceBinding dispatch, changes workload placement,
or deletes Nodes/NodeProvisions. See [HybridSpotVM integration](hybridspotvm-integration.md).
