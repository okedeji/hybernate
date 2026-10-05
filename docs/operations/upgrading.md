# Upgrading

## General Upgrade Process

1. **Read the release notes** in the [changelog](https://github.com/okedeji/hybernate/blob/main/CHANGELOG.md) for the target version. Check for breaking changes, CRD schema changes, or required steps.

2. **Upgrade.** The CRD is upgraded with the operator:

    === "Helm"

        ```bash
        helm upgrade hybernate oci://ghcr.io/okedeji/charts/hybernate \
          --version X.Y.Z \
          --namespace hybernate-system
        ```

        The chart is only published as an OCI artifact: there's no `helm repo` to update. Its version has no `v`. The CRD is a template in the chart, so `helm upgrade` upgrades it too, unless you installed with `crds.install: false` and manage it yourself.

    === "kubectl"

        ```bash
        kubectl apply -f https://github.com/okedeji/hybernate/releases/download/vX.Y.Z/install.yaml
        ```

3. **Verify the upgrade:**

    ```bash
    kubectl get pods -n hybernate-system
    kubectl logs -n hybernate-system -l control-plane=controller-manager --tail=20
    ```

4. **Check workloads:** Verify ManagedWorkloads are reconciling correctly:

    ```bash
    kubectl get managedworkloads --all-namespaces
    ```

## Upgrading from v0.1.x to v0.2.0

v0.2.0 is a relaunch with breaking changes to the ManagedWorkload API, and there's no migration: status is rebuilt, and the forecast learns again. See the [changelog](https://github.com/okedeji/hybernate/blob/main/CHANGELOG.md) for everything that changed. In particular:

- The `status.cost` fields are renamed and mean something different: they cover the current calendar month and are priced on requests. Anything that reads `currentMonth*Hours`, `estimatedMonthlyCost`, `estimatedMonthlySavings` or `estimatedCostWithoutManagement` must move to the [new fields](../concepts/cost-tracking.md#status-fields).
- The forecast's state is kept in `status.prediction.state`. The old `<name>-prediction-state` ConfigMaps are no longer read, and can be deleted.
- `prediction.confidence` must be at least 50.
- `desiredState: Destroyed` is gone; `desiredState` is `Running` or `Paused`.
- `spec.target` can no longer be changed once set.

**Before upgrading**, fix the ManagedWorkloads the new CRD would reject. The API server lets an unchanged invalid field through, but refuses any change to it until it's valid. This lists them:

```bash
kubectl get managedworkloads -A -o json | jq -r '.items[]
  | select(.spec.desiredState == "Destroyed" or (.spec.prediction.confidence // 100) < 50)
  | "\(.metadata.namespace)/\(.metadata.name)"'
```

Set each one's `desiredState` to `Paused` or remove it, and raise its `prediction.confidence` to at least 50:

```bash
kubectl patch managedworkload <name> -n <namespace> --type=json -p '[{"op":"remove","path":"/spec/desiredState"}]'
kubectl patch managedworkload <name> -n <namespace> --type=merge -p '{"spec":{"prediction":{"confidence":50}}}'
```

**With Helm**, the chart now installs and upgrades the CRD itself. A v0.1.7 install left the CRD outside the release, so let Helm adopt it once, before `helm upgrade`:

```bash
kubectl label crd managedworkloads.hybernate.io app.kubernetes.io/managed-by=Helm --overwrite
kubectl annotate crd managedworkloads.hybernate.io \
  meta.helm.sh/release-name=<release> \
  meta.helm.sh/release-namespace=<release-namespace> --overwrite
```

Then upgrade, from the OCI registry:

```bash
helm upgrade <release> oci://ghcr.io/okedeji/charts/hybernate --version 0.2.0 -n <release-namespace>
```

Check the new [Helm values](../reference/helm-values.md) as you do: logs are JSON by default (`logEncoder`), memory defaults are higher, and with secure metrics your Prometheus needs `metrics.readerSubjects` to scrape them; see [Monitoring](monitoring.md#prometheus).

**After upgrading**, with Helm or kubectl, delete the HybernateReport and WorkloadPolicy CRDs v0.1.x installed. v0.2.0 doesn't use them, and neither upgrade removes them. Deleting them deletes every HybernateReport and WorkloadPolicy; ManagedWorkloads a WorkloadPolicy created are left in place.

```bash
kubectl delete crd hybernatereports.hybernate.io workloadpolicies.hybernate.io
```

## CRD Compatibility

Hybernate follows these CRD versioning rules:

- **v1alpha1**: Breaking changes may occur between minor versions. Always read release notes.
- Field additions are non-breaking (new optional fields with defaults).
- Field removals or type changes are breaking and will be called out in release notes.

The CRD is kept when the chart is uninstalled, so uninstalling and reinstalling never deletes your ManagedWorkloads.

## Forecast Engine State

The forecast engine state is serialized in each ManagedWorkload's status, in `status.prediction.state`. On upgrade:

- Compatible state versions are imported automatically
- State that can't be read, from an incompatible version or a hand edit, is discarded, and the engine re-learns from scratch, with a `ForecastReset` warning event

## Rollback

If something goes wrong:

=== "Helm"

    ```bash
    helm rollback hybernate -n hybernate-system
    ```

=== "kubectl"

    ```bash
    kubectl apply -f https://github.com/okedeji/hybernate/releases/download/vPREVIOUS/install.yaml
    ```

Paused workloads remain paused during rollback. The previous operator version resumes managing them. Rolling back across a breaking API change, such as from v0.2.0 to v0.1.x, isn't supported.

## Version History

Check the [GitHub Releases](https://github.com/okedeji/hybernate/releases) page and the [changelog](https://github.com/okedeji/hybernate/blob/main/CHANGELOG.md) for every change.
