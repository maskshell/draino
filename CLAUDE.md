# Project: draino (Kubernetes node cordon/drain controller)

Draino watches Kubernetes Node resources and:

- Cordons immediately when a node matches the configured node selector
  (`--node-label-expr`) AND any configured node condition
  (`<node-conditions>...`) matches (optionally with required status +
  minimum duration).
- Drains later via eviction API, with a global `--drain-buffer` spacing
  between drain starts across the cluster.

## Quick orientation

- Entry point / CLI: `cmd/draino/draino.go`
- Core logic: `internal/kubernetes/`
  - `watch.go`: node informer/cache (30-minute resync period)
  - `eventhandler.go`: condition evaluation, cordon/uncordon, drain scheduling;
    `OnDelete` cleans up drain schedules
  - `drainSchedule.go`: rate-limited drain scheduler + retry mark/metrics/events
  - `drainer.go`: cordon + eviction implementation, sets Node conditions
  - `nodefilters.go`: `--node-label-expr` (antonmedv/expr) evaluation and legacy
    label conversion
  - `podfilters.go`: eviction eligibility filters (mirror/daemonset/statefulset/
    unreplicated/local-storage/protected-annotation)
  - `util.go`: Kubernetes client config, event recorder, `RetryWithTimeout` helper

## Runtime semantics to preserve

- Leader election (via `LeasesResourceLock` in `--namespace`, default
  `kube-system`) gates the node watcher — only the leader processes nodes.
  Params: `--leader-election-lease-duration` (15s),
  `--leader-election-renew-deadline` (10s),
  `--leader-election-retry-period` (2s).

- Cordon happens first, drain is scheduled:
  - Cordoning sets `spec.unschedulable=true` and writes the annotation
    `draino.planet.com/conditions` recording which conditions triggered the cordon.
  - Draining is scheduled for a future timestamp and tracked in-memory.

- `--drain-buffer` is global spacing, not per-node: only one drain start per
  buffer period (per draino instance), implemented by
  `DrainSchedules.lastDrainScheduledFor`.

- Condition state is persisted onto the Node:
  - Annotation `draino.planet.com/conditions` remembers triggering conditions
    for later uncordon decisions.
  - NodeCondition `DrainScheduled` shows scheduling + completion status
    (message includes schedule time and `Completed`/`Failed` timestamps).
  - `MarkDrain` calls use `RetryWithTimeout` (50ms interval, 10s timeout)
    to handle API conflicts.

- Drain retry: annotate `draino/drain-retry=true` to reschedule a failed
  drain. Retries loop forever if the annotation persists and drains keep
  failing.

- Pod eviction is concurrent; a single pod eviction failure causes the drain
  to be considered failed.

- PDB/TooManyRequests backoff: eviction API returning 429 triggers 5s sleep
  and retry loop.

- `OnDelete` removes pending drain schedules (no point draining a deleted
  node).

- Dry-run mode does not cordon or drain: uses `NoopCordonDrainer` and
  `NodeProcessed` filter (each node processed once to avoid repeating events).

## Configuration rules

### Node selection

- Prefer `--node-label-expr`. Evaluates an [antonmedv/expr] boolean
  expression against a `metadata.labels` map.
  - Example:
    `(metadata.labels.region == 'us-west-1' && metadata.labels.app == 'nginx')`
    `|| (metadata.labels.type == 'toolbox')`
  - Existence checks: `'<key>' in metadata.labels` or
    `metadata.labels.foo != ''`
- `--node-label` is deprecated and converted to an expression
  (`metadata.labels['k'] == 'v' && ...`). Do not extend legacy behavior.
- `--node-label` and `--node-label-expr` are mutually exclusive.

### Node conditions argument

`<node-conditions>...` are positional args and required. Each element is:

- Legacy: `ConditionType` (implies `Status=True`, `MinimumDuration=0s`)
- Extended: `ConditionType=Status,MinimumDuration`
  - Example: `ReadonlyFilesystem=True,5m`

Only nodes matching any supplied condition (after duration check) are handled.

### Draining / eviction behavior

Defaults are conservative:

- Default pod filter always excludes mirror pods.
- Without opt-in flags, draino refuses to evict:
  - DaemonSet pods (`--evict-daemonset-pods`)
  - StatefulSet pods (`--evict-statefulset-pods`)
  - pods with `emptyDir` (`--evict-emptydir-pods`)
  - unreplicated pods (`--evict-unreplicated-pods`)
- Protected annotations:
  - built-in: `cluster-autoscaler.kubernetes.io/safe-to-evict=false`
  - custom: `--protected-pod-annotation=KEY[=VALUE]` (repeatable)
- `--skip-drain`: cordons nodes but skips eviction (cordon-only mode).
- `--max-grace-period` (default 8m): max time for pods to terminate
  gracefully before SIGKILL.
- `--eviction-headroom` (default 30s): extra wait for API deletion
  confirmation; combined with `--max-grace-period` this sets the per-node
  drain deadline.

Changing any filter default is a behavioral breaking change for clusters.
Update `README.md` and tests accordingly.

### Leader election

- `--namespace` (default `kube-system`): namespace for the leader election
  lock Lease resource.
- `--leader-election-lease-duration` (default 15s)
- `--leader-election-renew-deadline` (default 10s)
- `--leader-election-retry-period` (default 2s)
- `--leader-election-token-name` (default `draino`): Lease resource name.

## Observability

- HTTP server: `--listen` (default `:10002`)
  - `/healthz`: liveness handler
  - `/metrics`: Prometheus exporter (OpenTelemetry, `draino_` namespace)
    - `draino_cordoned_nodes_total{result=...}`
    - `draino_uncordoned_nodes_total{result=...}`
    - `draino_drained_nodes_total{result=...}`
    - `draino_drain_scheduled_nodes_total{result=...}`

## Build, test, lint, Helm validation

### Local build

```bash
go build -o draino ./cmd/draino
docker build -t draino:test .   # Docker image
```

### Tests

```bash
./scripts/test.sh    # coverage output → output/coverage.txt
```

### Lint (CI uses golangci-lint v2 via golangci-lint-action@v7)

```bash
./bin/golangci-lint run --timeout 5m ./...
```

Install: `./scripts/install-tools.sh`

### Helm chart validation

Helm chart requires `conditions` to be set (unless `dryRun=true`).

```bash
helm template ./helm/draino/ --set 'conditions[0]=MemoryPressure' | ./bin/kubeconform --strict
```

Install: `./scripts/install-tools.sh`

### Run all CI checks locally

```bash
./scripts/install-tools.sh   # first time only
./scripts/ci-local.sh        # runs test + lint + helm-check + build
```

Templates must render without missing values and validate strictly.

## Deployment artifacts and conventions

- `manifest.yml`: example raw Kubernetes deployment (RBAC + Deployment).
- `helm/draino/`: maintained chart (values drive args).
- Container runs as non-root user; keep `readOnlyRootFilesystem: true`.

## Safety and change discipline

Changes affecting the following are behavioral and require unit test updates
under `internal/kubernetes/*_test.go` and `README.md` updates when
user-visible:

- filtering semantics (`--node-label-expr`, pod filters)
- scheduling semantics (`--drain-buffer`, retry)
- Node condition/annotation names or formats
- eviction mechanics/backoff

Avoid upgrading Kubernetes client-go or Go toolchain casually:

- The repo targets client-go v0.35.3 (Kubernetes 1.35, backward compatible
  with 1.36). Go 1.25+ is required.
- Upgrades require an explicit migration with a clear test plan.

## Working agreement for AI-assisted changes

- Identify whether a change is behavioral (affects drain decisions) or
  operational (logging/metrics/deploy only).
- Prefer small, test-backed edits in `internal/kubernetes/`.
- Keep flags and docs (`README.md`) consistent with code in
  `cmd/draino/draino.go`.

[antonmedv/expr]: https://github.com/antonmedv/expr
