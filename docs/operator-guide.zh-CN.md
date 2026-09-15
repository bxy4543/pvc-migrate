# PVC Migrate Operator 使用指南

本文介绍如何安装 PVC Migrate Operator，并通过声明式 Kubernetes 资源迁移指定 PVC。Helm Chart 的完整参数、升级和发布说明见 [Helm Chart 文档](../charts/pvc-migrate/README.md)。

## 1. 选择工作流资源

Operator 使用 `migrate.sealos.io/v1alpha1` CRD 保存请求、不可变执行计划和恢复检查点。

| 资源 | 用途 |
| --- | --- |
| `Migration` | 显式选择一个或多个 PVC，离线复制并把原 PVC 切换到新存储 |
| `PodMigration` | 选择一个 Pod，发现其挂载的全部 PVC，并管理支持的工作负载停机、预复制和恢复 |
| `Copy` | 把数据复制到另一个 PVC，不切换源 PVC 身份 |
| `Reservation` | 只创建并冻结目标卷，供后续复制使用 |
| `ClusterMigration` 等 `Cluster*` 资源 | 源、临时、目标或会话命名空间角色不同时，由集群管理员提交 |

`Cluster*` 表示同一 Kubernetes 集群内的跨命名空间角色，不表示跨集群迁移。跨 Kubernetes 集群工作流仍使用 CLI 的 `session` 后端。

本文主要使用 `Migration`。它只迁移 `spec.volumes` 中明确列出的 PVC，不会自动迁移同命名空间的其他 PVC。需要按工作负载自动发现所有 PVC 时使用 `PodMigration`。

## 2. 前置条件

- Kubernetes 1.25 或更高版本。
- Helm 3.17 或更高版本，或者 Helm 4。
- 安装者有权创建 CRD、ClusterRole 和 ClusterRoleBinding。
- release namespace、源 namespace、临时 namespace 和会话 namespace 均已存在。Operator 不创建 namespace。
- 源 PVC 已绑定，目标 StorageClass 可用，相关节点能够拉取工具镜像。
- 工具镜像应使用受信任的固定 tag。OCI 发布版默认使用与 Chart 相同版本的控制器和工具镜像。
- 迁移前已有应用级备份，并已确定停机、验证和回滚窗口。

## 3. 安装 Operator

### 3.1 从 OCI Chart 安装

先创建所需 namespace；Chart 不提供 `--create-namespace` 路径：

```bash
kubectl create namespace pvc-migrate-system
kubectl create namespace application

CHART_VERSION=X.Y.Z
helm upgrade --install pvc-migrate \
  oci://ghcr.io/labring-sigs/pvc-migrate/charts/pvc-migrate \
  --version "$CHART_VERSION" \
  --namespace pvc-migrate-system \
  --rollback-on-failure --wait --timeout 10m --history-max 10
```

Helm 3.17+ 使用 `--atomic` 替换 `--rollback-on-failure`。

### 3.2 从源码目录安装

```bash
helm lint ./charts/pvc-migrate --strict
helm template pvc-migrate ./charts/pvc-migrate \
  --namespace pvc-migrate-system --include-crds
helm upgrade --install pvc-migrate ./charts/pvc-migrate \
  --namespace pvc-migrate-system \
  --rollback-on-failure --wait --timeout 10m --history-max 10
```

### 3.3 验证安装

```bash
kubectl -n pvc-migrate-system rollout status deployment/pvc-migrate --timeout=5m
kubectl -n pvc-migrate-system get pods
kubectl get crd migrations.migrate.sealos.io podmigrations.migrate.sealos.io
helm test pvc-migrate -n pvc-migrate-system --logs --timeout 10m
```

默认运行两个控制器副本，由 leader election 保证只有 leader 执行工作流。第二个副本用于接管，不增加迁移并发量。

## 4. 迁移一个指定 PVC

以下请求只迁移 `application/data`。`metadata.namespace` 是 namespaced workflow 的租户边界；`sourcePVC` 等局部引用不接受 namespace 字段。

```yaml
apiVersion: migrate.sealos.io/v1alpha1
kind: Migration
metadata:
  name: migrate-data
  namespace: application
spec:
  destinationStorageClass: fast
  sourcePVReclaimPolicy: Retain
  destinationPVCReclaimPolicy: Retain
  volumes:
    - sourcePVC:
        name: data
```

```bash
kubectl apply -f migration.yaml
kubectl -n application get migration migrate-data -w
```

`Migration` 是离线 PVC 工作流，不负责缩容或停止任意未知消费者。执行前应确保规划检查能够识别并安全处理当前消费者；对于需要由 Operator 管理停机和恢复的工作负载，应使用 `PodMigration`。

## 5. 一次迁移多个指定 PVC

下面只迁移 `data` 和 `logs`。`cache` 等未列出的 PVC 不受影响：

```yaml
apiVersion: migrate.sealos.io/v1alpha1
kind: Migration
metadata:
  name: migrate-data-and-logs
  namespace: application
spec:
  destinationStorageClass: fast
  sourcePVReclaimPolicy: Retain
  destinationPVCReclaimPolicy: Retain
  volumes:
    - sourcePVC:
        name: data
    - sourcePVC:
        name: logs
      capacity: 200Gi
```

顶层 `destinationCapacity` 可为所有卷设置默认目标容量，单卷 `capacity` 会覆盖它。省略容量时默认继承源卷容量，但 HostPath 使用下节所述的安全容量。显式容量低于源容量时还需要满足缩容检查；显式 HostPath 容量低于安全下限会直接规划失败。

引用通常只需要 `name`。高敏感操作可增加 `uid` 或 `resourceVersion`，要求规划时对象仍为指定身份：

```yaml
volumes:
  - sourcePVC:
      name: data
      uid: 6e54e83f-1111-2222-3333-0123456789ab
```

## 6. 跨命名空间角色

当源、临时或会话 namespace 不同时，管理员可使用 cluster-scoped `ClusterMigration`：

```yaml
apiVersion: migrate.sealos.io/v1alpha1
kind: ClusterMigration
metadata:
  name: migrate-data
spec:
  sourceNamespace: application
  temporaryNamespace: pvc-migrate-work
  sessionNamespace: pvc-migrate-system
  destinationStorageClass: fast
  sourcePVReclaimPolicy: Retain
  destinationPVCReclaimPolicy: Retain
  volumes:
    - sourcePVC:
        name: data
```

三个 namespace 都必须预先存在。`sourcePVC.name` 相对于 `sourceNamespace`。集群级资源也可以用于同 namespace 场景，但应只授权给管理员。

## 7. HostPath 自动安全容量

静态 `PV.spec.hostPath` 和 OpenEBS Local PV HostPath 的 PVC 请求值可能只是声明值，实际文件可以超过它。Operator 会创建一个短生命周期 Pod，在卷所在节点只读挂载源 PVC，同时测量文件系统分配字节和表观字节，并取较大值以覆盖稀疏文件。

目标容量按以下规则计算：

```text
max(源 PV 声明容量, ceil(实际使用字节 x 1.20, 1MiB))
```

- 未指定目标容量时，Operator 自动使用该安全容量。
- 显式容量仍是用户要求，但低于安全容量时规划失败。
- 测量值和解析后的目标容量写入 `status.plan`，执行期间不会随 `spec` 漂移。
- final sync 前会重新测量；若数据增长已超过冻结的目标容量，工作流会在存储切换前失败。
- 例如声明容量为 `64Mi`、实际占用约 `80Mi`，目标至少约为 `96Mi`，最终值按 MiB 向上取整。
- 探测 Pod 必须能调度到源卷节点。静态 HostPath PV 没有 node affinity 时，Operator 无法证明调度位置，会拒绝规划。
- 规划和 CLI `--dry-run` 不会留下持久工作流或修改数据，但可能临时创建并删除该只读探测 Pod。

可以从冻结计划查看实际测量值和目标容量：

```bash
kubectl -n application get migration migrate-data \
  -o jsonpath='{range .status.plan.volumes[*]}{.sourcePVC.name}{" used="}{.sourceUsedBytes}{" capacity="}{.capacity}{"\n"}{end}'
```

## 8. 使用 PodMigration

`PodMigration` 以 Pod 为入口，自动发现该 Pod 挂载的全部 PVC，并解析受支持的工作负载控制器。`volumes` 是按源 PVC 名称提供的可选覆盖，不是筛选列表。

```yaml
apiVersion: migrate.sealos.io/v1alpha1
kind: PodMigration
metadata:
  name: migrate-database-0
  namespace: application
spec:
  pod:
    name: database-0
  destinationStorageClass: fast
  precopyPasses: 1
  sourcePVReclaimPolicy: Retain
  destinationPVCReclaimPolicy: Retain
```

如果目标是只迁移指定 PVC，不要用 `PodMigration`，应使用 `Migration.spec.volumes`。

`Copy` 接受 `volumes` 或 `pod`，只复制数据，不替换源 PVC；`Reservation` 只预创建目标存储。这两种资源只提供 `destinationPVCReclaimPolicy`，始终保留源存储。

## 9. 查看状态和执行计划

```bash
kubectl -n application get migration migrate-data -w
kubectl -n application get migration migrate-data -o yaml
kubectl -n application get migration migrate-data \
  -o jsonpath='{.status.phase}{"\n"}{.status.message}{"\n"}{.status.failureReason}{"\n"}'
kubectl -n application get events \
  --field-selector involvedObject.name=migrate-data --sort-by=.lastTimestamp
kubectl -n pvc-migrate-system logs deployment/pvc-migrate \
  --all-containers --since=30m
```

- `spec` 是用户提交的声明式意图。
- `status.plan` 是 Lease 保护下生成的不可变执行快照，包含 PVC/PV UID、目标容量、节点、工具镜像和工作负载快照。
- `status.phase` 是持久恢复点，`status.conditions`、`status.history`、`status.message` 和 `status.failureReason` 提供诊断信息。
- 常见阶段包括 `Planned`、`Reserving`、`Reserved`、`WarmCopying`、`Pausing`、`FinalSyncing`、`Activating`、`Completed`、`Failed`、`Aborting`、`Aborted`、`RollingBack` 和 `RolledBack`。

没有 `status.plan` 时，修正无效 `spec` 或外部依赖后可以重新规划。`status.plan` 一旦存在，修改容量、卷、节点或传输选项会被拒绝，应清理旧 workflow 并创建新资源。回收策略可在 cleanup 前修改，且不会改变冻结的传输计划。

## 10. 通过 CLI 提交和管理 Operator 工作流

CLI 的 `--mode=controller` 使用同一组 CRD。所有变更命令默认 `--dry-run=true`；实际执行必须显式指定 `--dry-run=false`。以下命令提交指定 PVC 并等待完成：

```bash
pvc-migrate --mode=controller --yes migrate \
  --source-namespace application \
  --source-pvc data \
  --destination-storage-class fast \
  --dry-run=false
```

增加 `--wait=false` 可只提交 CR，由其他进程观察。namespaced workflow 的生命周期命令使用 `--workflow-namespace` 定位资源：

```bash
pvc-migrate --mode=controller --workflow-namespace application \
  migrate status migrate-data

pvc-migrate --mode=controller --workflow-namespace application \
  migrate resume migrate-data --dry-run=false

pvc-migrate --mode=controller --workflow-namespace application --yes \
  migrate abort migrate-data --dry-run=false

pvc-migrate --mode=controller --workflow-namespace application --yes \
  migrate rollback migrate-data --dry-run=false

pvc-migrate --mode=controller --workflow-namespace application --yes \
  migrate cleanup migrate-data \
  --source-pv-reclaim-policy Retain \
  --destination-pvc-reclaim-policy Retain \
  --finalize --delete-session --dry-run=false
```

CLI 创建的 session ID 是 CR 名称；如果没有通过 `--session` 指定名称，应从命令输出或 `kubectl get migrations` 获取实际名称。

## 11. Resume、Abort、Rollback 和 Cleanup

| 操作 | 语义 |
| --- | --- |
| `resume` | 从失败检查点继续相同的冻结计划；不会重新解释已经冻结的迁移输入 |
| `abort` | 在激活前停止传输工具并恢复被暂停的工作负载，保留已分配存储供后续清理 |
| `rollback` | 在回滚窗口内把已迁移的 PVC 身份切回保留的源 PV |
| `cleanup` | 应用回收策略，释放工作流所有权、临时资源、Lease 和 finalizer，并关闭回滚窗口 |

直接删除 CR 不是强制删除。Operator 会通过 finalizer 执行取消、恢复和清理：

```bash
kubectl -n application delete migration migrate-data
```

激活开始前，删除会停止工具并恢复暂停的工作负载；激活已经开始时，会先完成存储切换再清理；若正在回滚，则先完成回滚。不要手工移除 finalizer，也不要通过删除 CRD 代替 workflow cleanup。

CR 长时间处于 `Terminating` 时，检查 `Deleting`、`DeletionBlocked` conditions、Events、控制器日志、消费者以及相关 Lease/Job/Pod。删除开始后，普通 CLI 生命周期变更会被拒绝。

## 12. 回收策略

`Migration` 和 `PodMigration` 的两个策略都默认为 `Retain`：

- `sourcePVReclaimPolicy` 只可能回收已完成迁移后不再活跃的原始 PV。回滚后源 PV 再次活跃，Operator 会保护它。
- `destinationPVCReclaimPolicy` 控制 cleanup 后是否删除剩余目标 PVC。
- `Copy` 和 `Reservation` 只有目标策略，源存储始终保留。
- `Delete` 目标仍被 Pod 使用时，cleanup 会阻塞。把策略改为 `Retain` 可以保留数据并允许 finalization 继续。

保守关闭回滚窗口并保留两端存储：

```bash
pvc-migrate --mode=controller --workflow-namespace application --yes \
  migrate cleanup migrate-data \
  --source-pv-reclaim-policy Retain \
  --destination-pvc-reclaim-policy Retain \
  --finalize --delete-session --dry-run=false
```

验证迁移结果后回收不活跃源 PV、保留当前目标：

```bash
pvc-migrate --mode=controller --workflow-namespace application --yes \
  migrate cleanup migrate-data \
  --source-pv-reclaim-policy Delete \
  --destination-pvc-reclaim-policy Retain \
  --finalize --delete-session --dry-run=false
```

执行 `Delete` 前确认底层 StorageClass/PV reclaim policy 和 CSI 驱动确实会按预期删除后端数据。

## 13. RBAC 和多租户边界

Helm Chart 安装的 ClusterRole 是高权限控制器身份，可管理存储对象、工作流状态和迁移工具。不得把它绑定给租户用户。

租户权限应限制在获准 namespace 内，只授予 workflow CR 的 `create/get/list/watch` 和 status 读取权限。`status` 更新、Secret 读取、PV 管理以及 `abort`、`rollback`、`cleanup`、失败任务恢复等操作应保留给控制器或运维身份。

每个集群只安装一套控制器 release。所有副本会观察所有已安装的 workflow CRD；在不同 namespace 重复安装控制器不能形成独立租户边界，并会共享固定的 leader-election Lease 约束。

## 14. 故障排查

| 现象 | 检查与处理 |
| --- | --- |
| 一直未生成 `status.plan` | 查看 `status.failureReason` 和 Events；检查 PVC/PV 绑定与 UID、StorageClass、配额、消费者、节点调度和权限 |
| HostPath usage Pod 为 Pending/Failed | 检查 PV node affinity、节点 taint/资源、工具镜像拉取、源文件读取权限；静态 HostPath 必须提供 node affinity |
| `Failed` 且没有 `status.plan` | 修正 `spec` 或外部依赖后执行 `resume`；此时允许重新规划 |
| `Failed` 且已有 `status.plan` | 修复对应外部问题后从冻结检查点 `resume`；不要修改迁移输入 |
| final sync 报目标容量不足 | 源数据已超过冻结容量；避免切换，终止并清理旧 workflow，再用更大的显式容量创建新 workflow |
| CR 卡在 `Terminating` | 检查 consumers、reclaim policy、Lease、孤儿 Job/Pod 和 deletion conditions；不要强删 finalizer |
| leader 切换后暂未继续 | 检查两个 controller Pod、日志和 leader-election Lease；接管时间受 Lease 过期和队列调度影响 |

列出由 Operator 创建的探测和传输 Pod：

```bash
kubectl get pods -A -l app.kubernetes.io/managed-by=pvc-migrate
```

## 15. 升级和卸载

升级前停止提交新任务，并完成、回滚或中止所有活动 workflow。先备份 CR 和关键存储对象，再按 [Chart 升级说明](../charts/pvc-migrate/README.md#upgrade-and-rollback) 审阅并应用新版本 CRD，最后升级控制器。

Helm 不会自动升级或删除 `crds/` 中的 CRD。`helm rollback` 只恢复控制器资源和 values，不会回滚 CRD schema、workflow 状态、PVC/PV 变更或数据。

卸载前必须完成 workflow 的 abort/rollback/cleanup：

```bash
helm uninstall pvc-migrate --namespace pvc-migrate-system --wait --timeout 10m
```

卸载保留 CRD、workflow CR、namespace 和应用存储，也不会替代迁移清理。未完成 workflow 的 finalizer 会一直保留，直到重新安装兼容控制器并完成恢复与 cleanup。
