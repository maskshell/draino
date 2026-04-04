# draino Helm Chart

Automatically cordons and drains Kubernetes nodes that match configured conditions.

## Prerequisites

- Kubernetes 1.33+

## Installing

```bash
helm install draino ./helm/draino \
  --set 'conditions[0]=MemoryPressure' \
  --set 'conditions[1]=Ready=False,5m'
```

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `dryRun` | bool | `false` | Emit events without cordoning or draining |
| `conditions` | list | `[]` | **Required**. Node conditions that trigger cordon/drain (e.g. `MemoryPressure`, `Ready=False,5m`) |
| `extraArgs` | list | `[]` | Extra CLI arguments passed to draino binary |
| `leaderElectionNamespace` | string | `kube-system` | Namespace for leader election lease |
| `replicaCount` | int | `1` | Number of replicas |
| `image.repository` | string | `ghcr.io/maskshell/draino` | Container image repository |
| `image.tag` | string | `""` (**required**) | Container image tag (e.g. a commit SHA) |
| `image.pullPolicy` | string | `IfNotPresent` | Image pull policy |
| `resources` | object | See values.yaml | Pod resource requests/limits |
| `rbac.create` | bool | `true` | Create ClusterRole and ClusterRoleBinding |
| `rbac.serviceAccountName` | string | `""` | Override service account name when `rbac.create=false` |
| `nodeSelector` | object | `{}` | Node selector for pod scheduling |
| `tolerations` | list | `[]` | Tolerations for pod scheduling |
| `affinity` | object | `{}` | Affinity rules for pod scheduling |
| `securityContext` | object | See values.yaml | Pod security context |
| `containerSecurityContext` | object | See values.yaml | Container security context |
| `podAnnotations` | object | `{}` | Annotations added to pods |
| `podLabels` | object | `{}` | Labels added to pods |

## Conditions

Each condition can be:

- Simple: `ConditionType` (implies Status=True, MinimumDuration=0s)
- Extended: `ConditionType=Status,MinimumDuration`

Example: `conditions[0]=Ready=False,5m`

## Dry-run mode

```bash
helm install draino ./helm/draino --set dryRun=true --set 'conditions[0]=MemoryPressure'
```

In dry-run mode, draino emits events but does not actually cordon or drain nodes.
