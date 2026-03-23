# GitOps Export Guide

Hybernate supports a GitOps workflow where you use the `kubectl hybernate export` plugin to generate ManagedWorkload manifests from a WorkloadPolicy's discoveries, commit them to Git, and deploy via ArgoCD or Flux.

## Why GitOps?

Auto-manage mode creates ManagedWorkloads directly in the cluster, which works well for getting started. But for production, you often want:

- **Auditability**: Git history shows who enabled management for which workload and when
- **Review process**: PR-based approval before managing a workload
- **Reproducibility**: Manifests in Git can recreate the same state on any cluster
- **Gradual rollout**: Review and customize individual ManagedWorkloads before applying

## Workflow

### 1. Deploy a WorkloadPolicy in Suggest Mode

```yaml title="workloadpolicy.yaml" linenums="1"
apiVersion: hybernate.io/v1alpha1
kind: WorkloadPolicy
metadata:
  name: staging-policy
  namespace: staging
spec:
  mode: suggest
  cpuIdleThreshold: 10
  memoryIdleThreshold: 10
  cpuWastefulThreshold: 30
```

```bash
kubectl apply -f workloadpolicy.yaml
```

### 2. Review Discoveries

```bash
kubectl get workloadpolicy staging-policy -n staging
```

```
NAME             MODE      DISCOVERED   ACTIVE   IDLE   WASTEFUL
staging-policy   suggest   12           8        2      2
```

### 3. Export to Files

```bash
kubectl hybernate export \
  --policy staging-policy \
  -n staging \
  --classification Idle \
  --output ./k8s/hybernate/staging/
```

This generates one YAML file per idle workload:

```
k8s/hybernate/staging/
├── idle-worker.yaml
└── legacy-api.yaml
```

Each file is a complete ManagedWorkload manifest with the policy's defaults applied.

### 4. Review and Customize

Open each manifest and adjust as needed:

```yaml title="k8s/hybernate/staging/idle-worker.yaml" linenums="1"
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata:
  name: idle-worker
  namespace: staging
  labels:
    hybernate.io/auto-discovered: "true"
  annotations:
    hybernate.io/workload-policy: staging-policy
spec:
  target:
    kind: Deployment
    name: idle-worker
  idlePolicy:
    action: pause
    cpuIdleThreshold: 10
    memoryIdleThreshold: 10
    gracePeriod: "5m"
    autoResume: true
  # ... other defaults from the policy
  dryRun: true  # Start safe
```

### 5. Commit and Deploy

```bash
git add k8s/hybernate/staging/
git commit -m "feat(hybernate): manage idle workloads in staging"
git push
```

ArgoCD or Flux syncs the manifests to the cluster. Once you're confident in the behavior (check events and status in dry-run mode), flip `dryRun` to `false` via a follow-up PR.

## Graduating from Auto-Manage to GitOps

If you started with `auto-manage` mode and want to move to GitOps:

1. Export including already-managed workloads:

```bash
kubectl hybernate export \
  --policy staging-policy \
  -n staging \
  --include-managed \
  --output ./k8s/hybernate/staging/
```

2. Commit the exported manifests
3. Switch the WorkloadPolicy back to `suggest` mode
4. Delete the auto-created ManagedWorkloads (the GitOps manifests will recreate them)

## Tips

- **Start with `dryRun: true`** in exported manifests. Review events before enabling.
- **Use `--classification`** to export in batches: idle workloads first, wasteful later.
- **Customize per-workload** when defaults don't fit. Adjust grace periods, idle thresholds, or add Prometheus signals as needed.
- **Keep the WorkloadPolicy in suggest mode** alongside GitOps. It continues scanning and reporting new discoveries without creating anything automatically.
