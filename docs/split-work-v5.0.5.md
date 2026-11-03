# Split PV/PVC Works v5.0.5

Karmada remains unchanged. Each standalone claim now produces one PV Work and
one PVC Work. Template claims retain their existing PV-only Work path.
Both standalone Works must reflect Bound, matching migration identity, exact
NFS and reciprocal PV/PVC binding. Their member UIDs are persisted in
PVMigration status before cleanup. Cleanup checks these receipts and preserves
the actual PV/PVC resources.

Apply the updated PVMigration CRD before deploying this image. Upgrade System
policy-manager and placement-webhook to v5.0.5 as well: their restore gate now
requires two matching receipts per standalone claim.

Existing combined-Work migration plans are not converted in place. Retain their
diagnostics and use a new experiment RUN after normal cleanup. Do not manually
mark them Completed. Do not apply the old custom Karmada image.

The complete coordinated deployment instructions are in the System repository:
`docs/e1-split-work-v5.0.5.md`.
