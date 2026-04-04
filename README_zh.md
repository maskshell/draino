# draino [![CI](https://github.com/maskshell/draino/actions/workflows/ci.yml/badge.svg)](https://github.com/maskshell/draino/actions/workflows/ci.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/maskshell/draino.svg)](https://pkg.go.dev/github.com/maskshell/draino) [![Codecov](https://img.shields.io/codecov/c/github/maskshell/draino.svg?maxAge=3600)](https://codecov.io/gh/maskshell/draino/)

Draino 基于标签和节点条件自动驱逐 Kubernetes 节点。匹配所有指定标签且满足任一节点条件的节点将被立即隔离（cordon），并在可配置的 `drain-buffer` 时间后执行驱逐（drain）。

Draino 旨在配合 Kubernetes [Node Problem Detector](https://github.com/kubernetes/node-problem-detector)
和 [Cluster Autoscaler](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler) 使用。
Node Problem Detector 可以在检测到节点异常时设置节点条件——例如通过监控节点日志或运行脚本。Cluster Autoscaler 可以配置为删除利用率不足的节点。
将 Draino 加入其中可实现自动修复：

1. Node Problem Detector 检测到永久性节点问题并设置相应的节点条件。
2. Draino 感知到节点条件后，立即隔离该节点以防止新 Pod 被调度到该节点，并调度一次节点驱逐。
3. 节点被驱逐后，Cluster Autoscaler 会将其视为利用率不足。经过一段可配置的时间后，该节点将符合缩容（即终止）条件。

## 用法

```bash
docker run ghcr.io/maskshell/draino /draino --help
```

输出：

```text
usage: draino [<flags>] <node-conditions>...

Automatically cordons and drains nodes that match the supplied conditions.

Flags:
      --help                     Show context-sensitive help (also try --help-long and --help-man).
  -d, --debug                    Run with debug logging.
      --listen=":10002"          Address at which to expose /metrics and /healthz.
      --kubeconfig=KUBECONFIG    Path to kubeconfig file. Leave unset to use in-cluster config.
      --master=MASTER            Address of Kubernetes API server. Leave unset to use in-cluster config.
      --dry-run                  Emit an event without cordoning or draining matching nodes.
      --max-grace-period=8m0s    Maximum time evicted pods will be given to terminate gracefully.
      --eviction-headroom=30s    Additional time to wait after a pod's termination grace period for it to have been deleted.
      --drain-buffer=10m0s       Minimum time between starting each drain. Nodes are always cordoned immediately.
      --node-label="foo=bar"     (DEPRECATED) Only nodes with this label will be eligible for cordoning and draining. May be specified multiple times.
      --node-label-expr="metadata.labels.foo == 'bar'"
                                 This is an expr string https://github.com/antonmedv/expr that must return true or false. See `nodefilters_test.go` for examples
      --namespace="kube-system"  Namespace used to create leader election lock object.
      --leader-election-lease-duration=15s
                                 Lease duration for leader election.
      --leader-election-renew-deadline=10s
                                 Leader election renew deadline.
      --leader-election-retry-period=2s
                                 Leader election retry period.
      --skip-drain               Whether to skip draining nodes after cordoning.
      --evict-daemonset-pods     Evict pods that were created by an extant DaemonSet.
      --evict-emptydir-pods      Evict pods with local storage, i.e. with emptyDir volumes.
      --evict-unreplicated-pods  Evict pods that were not created by a replication controller.
      --protected-pod-annotation=KEY[=VALUE] ...
                                 Protect pods with this annotation from eviction. May be specified multiple times.

Args:
  <node-conditions>  Nodes for which any of these conditions are true will be cordoned and drained.
```

## 环境要求

* Kubernetes 1.33+（向后兼容 1.34 和 1.35）
* Go 1.25+（源码构建时需要）

## 构建

```bash
go build -o draino ./cmd/draino
docker build -t draino:test .
```

CI 使用 Go 1.25 和 golangci-lint v1.62.0。详情参见 [.github/workflows/ci.yml](.github/workflows/ci.yml)。

### 标签与标签表达式

Draino 允许使用 `--node-label` 和 `--node-label-expr` 过滤符合条件的节点集合。
原始标志 `--node-label` 仅支持指定标签的布尔与运算。为表达更复杂的谓词，新增的 `--node-label-expr`
标志支持通过 <https://github.com/antonmedv/expr> 实现混合 OR/AND/NOT 逻辑。

`--node-label-expr` 示例：

```text
(metadata.labels.region == 'us-west-1' && metadata.labels.app == 'nginx') || (metadata.labels.region == 'us-west-2' && metadata.labels.app == 'nginx')
```

## 注意事项

部署 Draino 前请注意以下事项：

* 务必先以 `--dry-run` 模式运行 Draino，确保其驱逐的节点符合预期。在 dry run 模式下，Draino 会输出日志、指标和事件，但不会实际执行隔离或驱逐操作。
* Draino 会立即隔离匹配其配置标签和节点条件的节点，但会在节点驱逐之间等待一段可配置的时间（默认 10 分钟）。即，如果两个节点同时出现节点条件，一个节点将被立即驱逐，另一个将在 10 分钟后驱逐。
* Draino 认为只要至少有一个 Pod 被驱逐失败，整个驱逐即视为失败。如果 Draino 未能驱逐 5 个 Pod 中的 2 个，驱逐将被标记为失败，但剩余 3 个 Pod 始终会被驱逐。
* Cluster Autoscaler 无法驱逐的 Pod，Draino 也无法驱逐。
  参见 [Cluster Autoscaler 文档](https://github.com/kubernetes/autoscaler/blob/master/cluster-autoscaler/FAQ.md#what-types-of-pods-can-prevent-ca-from-removing-a-node)中的注解 `"cluster-autoscaler.kubernetes.io/safe-to-evict": "false"`。

## 部署

Draino 自动从 master 分支构建并推送至 [GHCR](https://github.com/maskshell/draino/pkgs/container/draino)。
构建产物标签为 `ghcr.io/maskshell/draino:$(git rev-parse --short HEAD)`。

提供了[示例 Kubernetes 部署清单](manifest.yml)。

## 监控

### 指标

Draino 在 `/healthz` 提供健康检查端点，在 `/metrics` 提供 Prometheus 指标（通过 OpenTelemetry）。现有指标如下：

```bash
$ kubectl -n kube-system exec -it ${DRAINO_POD} -- apk add curl
$ kubectl -n kube-system exec -it ${DRAINO_POD} -- curl http://localhost:10002/metrics
# HELP draino_cordoned_nodes_total Number of nodes cordoned.
# TYPE draino_cordoned_nodes_total counter
draino_cordoned_nodes_total{result="succeeded"} 2
draino_cordoned_nodes_total{result="failed"} 1
# HELP draino_drained_nodes_total Number of nodes drained.
# TYPE draino_drained_nodes_total counter
draino_drained_nodes_total{result="succeeded"} 1
draino_drained_nodes_total{result="failed"} 1
```

### 事件

Draino 会为驱逐过程中的每个相关步骤生成事件。以下是一个以 `DrainFailed` 结束的示例。当一切正常时，给定节点的最后一个事件的原因为 `DrainSucceeded`。

```bash
kubectl get events -n default | grep -E '(^LAST|draino)'
```

输出：

```text
LAST SEEN   FIRST SEEN   COUNT   NAME                                               KIND TYPE      REASON             SOURCE MESSAGE
5m          5m           1       node-demo.15fe0c35f0b4bd10    Node Warning   CordonStarting     draino Cordoning node
5m          5m           1       node-demo.15fe0c35fe3386d8    Node Warning   CordonSucceeded    draino Cordoned node
5m          5m           1       node-demo.15fe0c360bd516f8    Node Warning   DrainScheduled     draino Will drain node after 2020-03-20T16:19:14.91905+01:00
5m          5m           1       node-demo.15fe0c3852986fe8    Node Warning   DrainStarting      draino Draining node
4m          4m           1       node-demo.15fe0c48d010ecb0    Node Warning   DrainFailed        draino Draining failed: timed out waiting for evictions to complete: timed out
```

### 节点条件

当驱逐被调度时，除了事件外，还会在节点状态中添加一个条件。该条件将包含驱逐过程开始和结束的信息。可以通过描述节点资源来查看：

```bash
kubectl describe node {node-name}
```

```text
......
Unschedulable:      true
Conditions:
  Type                  Status  LastHeartbeatTime                 LastTransitionTime                Reason                       Message
  ----                  ------  -----------------                 ------------------                ------                       -------
  OutOfDisk             False   Fri, 20 Mar 2020 15:52:41 +0100   Fri, 20 Mar 2020 14:01:59 +0100   KubeletHasSufficientDisk     kubelet has sufficient disk space available
  MemoryPressure        False   Fri, 20 Mar 2020 15:52:41 +0100   Fri, 20 Mar 2020 14:01:59 +0100   KubeletHasSufficientMemory   kubelet has sufficient memory available
  DiskPressure          False   Fri, 20 Mar 2020 15:52:41 +0100   Fri, 20 Mar 2020 14:01:59 +0100   KubeletHasNoDiskPressure     kubelet has no disk pressure
  PIDPressure           False   Fri, 20 Mar 2020 15:52:41 +0100   Fri, 20 Mar 2020 14:01:59 +0100   KubeletHasSufficientPID      kubelet has sufficient PID available
  Ready                 True    Fri, 20 Mar 2020 15:52:41 +0100   Fri, 20 Mar 2020 14:02:09 +0100   KubeletReady                 kubelet is posting ready status. AppArmor enabled
  ec2-host-retirement   True    Fri, 20 Mar 2020 15:23:26 +0100   Fri, 20 Mar 2020 15:23:26 +0100   NodeProblemDetector          Condition added with tooling
  DrainScheduled        True    Fri, 20 Mar 2020 15:50:50 +0100   Fri, 20 Mar 2020 15:23:26 +0100   Draino                       Drain activity scheduled 2020-03-20T15:50:34+01:00
```

当驱逐活动完成后，该条件将被更新，告知你驱逐是成功还是失败：

```bash
kubectl describe node {node-name}
```

```text
......
Unschedulable:      true
Conditions:
  Type                  Status  LastHeartbeatTime                 LastTransitionTime                Reason                       Message
  ----                  ------  -----------------                 ------------------                ------                       -------
  OutOfDisk             False   Fri, 20 Mar 2020 15:52:41 +0100   Fri, 20 Mar 2020 14:01:59 +0100   KubeletHasSufficientDisk     kubelet has sufficient disk space available
  MemoryPressure        False   Fri, 20 Mar 2020 15:52:41 +0100   Fri, 20 Mar 2020 14:01:59 +0100   KubeletHasSufficientMemory   kubelet has sufficient memory available
  DiskPressure          False   Fri, 20 Mar 2020 15:52:41 +0100   Fri, 20 Mar 2020 14:01:59 +0100   KubeletHasNoDiskPressure     kubelet has no disk pressure
  PIDPressure           False   Fri, 20 Mar 2020 15:52:41 +0100   Fri, 20 Mar 2020 14:01:59 +0100   KubeletHasSufficientPID      kubelet has sufficient PID available
  Ready                 True    Fri, 20 Mar 2020 15:52:41 +0100   Fri, 20 Mar 2020 14:02:09 +0100   KubeletReady                 kubelet is posting ready status. AppArmor enabled
  ec2-host-retirement   True    Fri, 20 Mar 2020 15:23:26 +0100   Fri, 20 Mar 2020 15:23:26 +0100   NodeProblemDetector          Condition added with tooling
  DrainScheduled        True    Fri, 20 Mar 2020 15:50:50 +0100   Fri, 20 Mar 2020 15:23:26 +0100   Draino                       Drain activity scheduled 2020-03-20T15:50:34+01:00 | Completed: 2020-03-20T15:50:50+01:00
  ```

如果驱逐失败，条件行将显示为：

```text
  DrainScheduled        True    Fri, 20 Mar 2020 15:50:50 +0100   Fri, 20 Mar 2020 15:23:26 +0100   Draino                       Drain activity scheduled 2020-03-20T15:50:34+01:00| Failed:2020-03-20T15:55:50+01:00
```

## 重试驱逐

在某些情况下，驱逐可能因限制性 Pod Disruption Budget 或 Draino 外部的其他原因而失败。节点保持隔离状态，驱逐条件被标记为 `Failed`。如需对该节点重新调度驱逐尝试，请添加注解：`draino/drain-retry: true`。将创建新的驱逐调度计划。注意该注解不会被修改，如果驱逐再次失败将触发循环重试。

```bash
kubectl annotate node {node-name} draino/drain-retry=true
```

## 运行模式

### Dry Run（试运行）

Draino 可以使用 `--dry-run` 标志以试运行模式运行。

### 仅隔离（Cordon Only）

Draino 也可以选择以仅隔离模式运行，即只隔离节点而不执行驱逐。可通过 `--skip-drain` 标志实现。
