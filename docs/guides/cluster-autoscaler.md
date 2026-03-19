# Cluster Autoscaler Configuration

Hybernate manages workloads at the pod layer: it pauses, scales, and destroys idle Deployments and StatefulSets. But pods run on nodes, and nodes cost money. For Hybernate's cost savings to materialize, your cluster needs an autoscaler that removes underutilized nodes after Hybernate frees up capacity.

```
Hybernate scales down pods → Nodes become underutilized → Autoscaler removes empty nodes → Cost savings
```

Without an autoscaler, scaling pods to zero still leaves the underlying node running at full price.

This guide covers recommended autoscaler settings for the two most common options.

## Cluster Autoscaler

[Cluster Autoscaler](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler) manages node groups across EKS, GKE, AKS, and self-managed clusters.

### Recommended Settings

```yaml title="cluster-autoscaler-values.yaml" linenums="1"
autoDiscovery:
  clusterName: my-cluster

extraArgs:
  # How long after scale-down before another node can be removed.
  # Keep this shorter than Hybernate's default reconcile interval (1m)
  # so freed capacity is reclaimed promptly.
  scale-down-delay-after-delete: 1m

  # How long a node must be underutilized before it's eligible for removal.
  # 5m works well with Hybernate's grace period (default 5m).
  # If this is longer than the grace period, nodes linger after pods are gone.
  scale-down-unneeded-time: 5m

  # CPU utilization threshold below which a node is considered underutilized.
  # 0.5 means a node using less than 50% CPU is a candidate for removal.
  scale-down-utilization-threshold: "0.5"

  # Prevents thrashing when Hybernate resumes workloads.
  # After a scale-up, wait before considering scale-down again.
  scale-down-delay-after-add: 5m

  # How often the autoscaler evaluates the cluster.
  # 10s is the default. No need to change for Hybernate.
  scan-interval: 10s
```

### Key Interactions

| Hybernate Action | Autoscaler Response |
|-----------------|-------------------|
| Pause (scale to 0) | Pods removed, node becomes underutilized, removed after `scale-down-unneeded-time` |
| Scale down (reduce replicas) | Freed capacity may make node underutilized, removed if below threshold |
| Resume (scale back up) | Autoscaler provisions new nodes if no capacity exists |
| Destroy (delete workload) | Same as pause, node removed if underutilized |

### Things to Watch

- **`scale-down-unneeded-time` vs Hybernate's grace period**: If the autoscaler's unneeded time is much longer than Hybernate's grace period, nodes will sit idle after Hybernate pauses workloads. Align them or keep the autoscaler's shorter.
- **Pod Disruption Budgets**: The autoscaler must evict pods before removing a node, and PDBs can block eviction. Hybernate bypasses this because it scales replica counts directly, not through eviction. However, non-managed pods with PDBs on the same node can still prevent the autoscaler from removing it.
- **DaemonSets**: Nodes running only DaemonSet pods are still considered underutilized and can be removed. This is usually the desired behavior.

## Karpenter

[Karpenter](https://karpenter.sh) is a CNCF node autoscaler with native support on EKS and Azure (via AKS Node Autoprovision). Instead of managing node groups, it provisions individual nodes sized to fit pending pods and consolidates underutilized nodes proactively.

### Recommended Settings

=== "v1 (Karpenter 1.x)"

    ```yaml title="nodepool.yaml" linenums="1"
    apiVersion: karpenter.sh/v1
    kind: NodePool
    metadata:
      name: default
    spec:
      disruption:
        # Karpenter will consolidate underutilized nodes by moving pods
        # and terminating the emptied node. This is the key setting for
        # Hybernate: when pods are paused/removed, Karpenter consolidates.
        consolidationPolicy: WhenEmptyOrUnderutilized

        # How long a node must be empty or underutilized before consolidation.
        # 30s is aggressive but works well with Hybernate since the scale-down
        # is intentional, not transient.
        consolidateAfter: 30s

        # Maximum time a node can live, regardless of utilization.
        # Useful for picking up new AMIs and preventing drift.
        expireAfter: 720h  # 30 days

      template:
        spec:
          requirements:
            - key: karpenter.sh/capacity-type
              operator: In
              values: ["on-demand", "spot"]
            - key: kubernetes.io/arch
              operator: In
              values: ["amd64"]
    ```

=== "v1beta1 (Karpenter 0.x)"

    ```yaml title="nodepool.yaml" linenums="1"
    apiVersion: karpenter.sh/v1beta1
    kind: NodePool
    metadata:
      name: default
    spec:
      disruption:
        consolidationPolicy: WhenUnderutilized

        # v1beta1 uses expireAfter at this level
        expireAfter: 720h

      template:
        spec:
          requirements:
            - key: karpenter.sh/capacity-type
              operator: In
              values: ["on-demand", "spot"]
    ```

### Key Interactions

| Hybernate Action | Karpenter Response |
|-----------------|-------------------|
| Pause (scale to 0) | Node becomes empty, consolidated after `consolidateAfter` |
| Scale down (reduce replicas) | Remaining pods may fit on fewer nodes, Karpenter consolidates |
| Resume (scale back up) | Karpenter provisions a right-sized node for the pending pods |
| Destroy (delete workload) | Same as pause, node consolidated if empty/underutilized |

### Why Karpenter Works Well With Hybernate

- **Right-sizing on resume**: When Hybernate resumes a workload, Karpenter provisions a node sized to fit the exact pod requirements rather than falling back to a fixed node group size. This avoids over-provisioning on resume.
- **Fast consolidation**: Karpenter's consolidation is faster than Cluster Autoscaler's scale-down, so cost savings from Hybernate's actions are realized sooner.
- **Spot instances**: Karpenter's native spot support pairs well with Hybernate's forecast-driven scheduling. Non-critical workloads that Hybernate manages are good candidates for spot capacity.

## Autopilot and Serverless Modes

Some managed Kubernetes offerings handle node scaling transparently:

| Service | How It Works | Hybernate Value |
|---------|-------------|-----------------|
| **GKE Autopilot** | Google manages nodes entirely. You pay per pod resource request. | Direct cost savings. Every pod Hybernate removes reduces the bill immediately. No autoscaler configuration needed. |
| **EKS with Fargate** | AWS runs each pod on a dedicated microVM. No shared nodes. | Direct cost savings. Pausing a pod stops billing for that pod. No autoscaler configuration needed. |
| **AKS Node Autoprovision** | Azure manages nodes using Karpenter under the hood. | Use the Karpenter settings above. Azure handles the rest. |

These are the simplest setups for Hybernate because the cost savings are 1:1 with pod scaling: no pods running means no cost.

## Verifying It Works

After configuring your autoscaler, verify the end-to-end flow:

1. **Pause a workload:**

    ```bash
    kubectl patch managedworkload my-api -n staging \
      --type merge -p '{"spec":{"desiredState":"Paused"}}'
    ```

2. **Watch node count:**

    ```bash
    kubectl get nodes -w
    ```

3. **Confirm a node is removed** after the autoscaler's configured delay.

4. **Resume the workload:**

    ```bash
    kubectl patch managedworkload my-api -n staging \
      --type merge -p '{"spec":{"desiredState":"Running"}}'
    ```

5. **Confirm a node is provisioned** and the workload pods become ready.

If nodes are not being removed after pause, check:

- Autoscaler logs for why scale-down was skipped
- Whether non-managed pods on the same node prevent removal
- Whether PDBs are blocking eviction
- Whether the utilization threshold is set too low
