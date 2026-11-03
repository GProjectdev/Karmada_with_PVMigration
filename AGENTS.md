# Compatibility Constraints

- Keep upstream Karmada unchanged. Fix integration issues in this repository, not in a Karmada fork or controller-manager image.
- Emit one resource manifest per Work. Standalone claims require separate PV and PVC Works and reciprocal binding receipts.
- Never bypass UID, NFS, ownership, source fencing, or restore gates.
- Use Buildah for container builds and publishing. Document full kubectl commands, not aliases.
