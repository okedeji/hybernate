# Upgrading

## General Upgrade Process

1. **Read the release notes** for the target version — check for breaking changes, CRD schema changes, or required migrations.

2. **Update CRDs first** — CRD changes must be applied before upgrading the operator:

    ```bash
    kubectl apply -f https://github.com/okedeji/hybernate/releases/download/vX.Y.Z/install.yaml
    ```

    Or if using Helm:

    ```bash
    helm repo update
    helm upgrade hybernate hybernate/hybernate \
      --namespace hybernate-system
    ```

3. **Verify the upgrade:**

    ```bash
    kubectl get pods -n hybernate-system
    kubectl logs -n hybernate-system deployment/hybernate-controller-manager | head -20
    ```

4. **Check workloads:** Verify ManagedWorkloads are reconciling correctly:

    ```bash
    kubectl get managedworkloads --all-namespaces
    ```

## CRD Compatibility

Hybernate follows these CRD versioning rules:

- **v1alpha1**: Breaking changes may occur between minor versions. Always read release notes.
- Field additions are non-breaking (new optional fields with defaults).
- Field removals or type changes are breaking and will be called out in release notes.

## Forecast Engine State

The forecast engine state is serialized in each ManagedWorkload's status. On upgrade:

- Compatible state versions are imported automatically
- Incompatible versions cause the engine to reset and re-learn from scratch (a warning event is emitted)

## Rollback

If something goes wrong:

```bash
# Helm
helm rollback hybernate -n hybernate-system

# kubectl
kubectl apply -f https://github.com/okedeji/hybernate/releases/download/vPREVIOUS/install.yaml
```

Paused workloads remain paused during rollback. The previous operator version resumes managing them.

## Version History

Check the [GitHub Releases](https://github.com/okedeji/hybernate/releases) page for the full changelog.
