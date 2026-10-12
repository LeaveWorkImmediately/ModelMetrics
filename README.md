# ModelMetrics

ModelMetrics is ModelPilot's internal metrics storage and query capability. The MVP uses one
Prometheus instance, but consumers address it as `model-metrics` rather than depending on the
implementation name.

## 部署组成与数据链路

ModelMetrics 主服务、Exporter 和设备 Agent 是独立工作负载。设备前端闭环还需要更新
ModelDashBoard、ModelPilotMetaRedis ACL 和 ModelPilotPortal。

| 组件 | 部署形式 | Namespace | 镜像来源 / 部署入口 |
|---|---|---|---|
| ModelMetrics / Prometheus | 单副本 Deployment + PVC | `monitoring` | `quay.io/prometheus/prometheus:v2.45.4`；[主清单](deploy/kubernetes.yaml) |
| node_exporter | DaemonSet | `monitoring` | `quay.io/prometheus/node-exporter:v1.8.2`；[节点采集清单](deploy/node-exporter/kubernetes.yaml) |
| Ascend NPU Exporter | DaemonSet | `npu-exporter` | 安装时指定已验证镜像；[Ascend 清单](deploy/exporters/ascend/kubernetes.yaml) |
| 设备 Agent | DaemonSet | `monitoring` | [Git 构建入口](agent/Dockerfile.git)；[Agent 清单](deploy/device-agent/kubernetes.yaml) |

```text
Exporter → Prometheus → Dashboard → Portal
设备 Agent → Dashboard 设备事实库 → Redis metadata
```

`agent/` 包含 Go session/sequence、重试、projected token 更新和健康探针，以及有超时和输出
预算的 Python DCMI 适配器。Agent 与 Portal 不持有 Redis 凭据；Dashboard 发布 Device/Node
描述到现有 metadata/v1。PodResources 与分配历史留待后续阶段。

## 1. 准备安装配置

以下 ModelMetrics 命令默认在本仓根目录执行。清单是安装模板，执行前先完成环境参数替换：

- Prometheus PVC 默认 `nfs-client`、`50Gi`，按集群 StorageClass 和容量调整；指标保留期默认 15 天。
- Dashboard `METADATA_SCOPE`、Agent `CLUSTER_ID`、Prometheus `cluster_id` 与 Redis ACL scope
  必须一致，模板默认 `installation-local`。
- 替换 Agent 和 NPU Exporter 镜像占位符，使用不可变 tag/digest。私有仓库配置节点拉取凭据或
  Pod `imagePullSecrets`；构建期 `GIT_TOKEN` 不用于运行时镜像拉取。
- 核对 Ascend 驱动、DCMI library 和 containerd socket。模板使用
  `/usr/local/Ascend/driver`、`/usr/local/dcmi` 和 K3s `/run/k3s/containerd`；其他环境调整挂载和参数。
- 修改 `NO_PROXY` 和 NetworkPolicy 的 Kubernetes API 地址/端口。Agent/Dashboard 模板为
  `10.43.0.1/32:443`，若实际 endpoint 使用 `6443`，补充对应地址与端口。
- 主清单的 API 与 node_exporter 出站 CIDR 当前为 `0.0.0.0/0`；安装时分别限定到实际 API
  和选定节点地址。node_exporter 使用 hostNetwork，验证节点 `9100` 端口、宿主防火墙和 CNI。

## 2. 构建设备 Agent 镜像

### Ouroboros / Kaniko Git 构建

先将代码提交并推送到目标分支。在 Ouroboros 关联 **ModelMetrics 仓库**，使用
`agent/Dockerfile.git` 的内容作为在线构建入口；它自行 clone 仓库并在 `/source/agent` 构建。

| 构建参数 | 配置 |
|---|---|
| `GIT_REPO_URL` | ModelMetrics 仓库 URL，必填，文件不内置远程仓库地址 |
| `GIT_REF` | 已推送分支或 tag，默认 `main` |
| `GIT_TOKEN` | 私有 HTTPS 仓库凭据，public 仓库可为空 |
| `CACHEBUST` | 同一分支重复构建时更新，避免复用旧 clone 层 |
| `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` | 按构建环境配置或由平台注入 |

输出镜像使用 `REGISTRY/modelpilot/model-device-agent:IMMUTABLE_TAG` 或 digest，并回填到
`deploy/device-agent/kubernetes.yaml`。Dockerfile.git 完成镜像构建；工作负载仍按 DaemonSet 清单部署。

### 本地构建

两种构建入口使用相同运行镜像和 DCMI 适配器。本地 build context 为 `agent`：

```sh
docker build -f agent/Dockerfile \
  -t REGISTRY/modelpilot/model-device-agent:IMMUTABLE_TAG agent
docker push REGISTRY/modelpilot/model-device-agent:IMMUTABLE_TAG
```

Prometheus、node_exporter 使用现成镜像；NPU Exporter 镜像是安装输入，当前没有对应的
ModelMetrics Dockerfile.git。

## 3. 部署 Prometheus 主服务

```sh
kubectl apply -f deploy/kubernetes.yaml
kubectl -n monitoring get pvc model-metrics-data
kubectl -n monitoring rollout status deployment/model-metrics
```

清单包含 namespace、ConfigMap、服务发现 RBAC、PVC、Deployment、Service 和 NetworkPolicy。
当前单副本使用 `Recreate` 策略，CPU request/limit 为 `500m/2`，内存为 `2Gi/4Gi`。

内部查询地址：

```text
http://model-metrics.monitoring.svc.cluster.local:9090
```

Service 为 ClusterIP，无 NodePort/Ingress。采集周期和规则评估周期均为 15 秒；自动发现
`default` 中 ModelFlow Router 的 `metrics` 端口、`monitoring` 中 node_exporter 和
`npu-exporter` namespace 中 NPU Exporter 的 `8082` 端口。

## 4. 部署 Exporter 并完成真机校验

先指定允许采集的节点。三个 DaemonSet 都使用此 nodeSelector：

```sh
kubectl label node NODE_NAME modelpilot.io/device-observation=true --overwrite
kubectl apply -f deploy/node-exporter/kubernetes.yaml
kubectl apply -f deploy/exporters/ascend/kubernetes.yaml
kubectl -n monitoring rollout status daemonset/model-node-exporter
kubectl -n npu-exporter rollout status daemonset/npu-exporter
```

Ascend 模板参考 mind-cluster commit `7537ed628111e89f82e4823cd36b2c983d86ab6b`。
Agent/NPU Exporter 使用 privileged 与驱动挂载，设备访问能力需要在选定真机上验证。

对照 DCMI 与 Exporter 的物理 ID、物理/虚拟层级和单位，并记录到
[Exporter profile](contracts/ascend-exporter-profiles.yaml)：

- Exporter `id` 对应 DCMI physical ID，虚拟设备不能混入物理清单。
- 利用率为百分数，HBM 为 MiB，温度为摄氏度，功耗为芯片 W；310P 卡级功耗不能当作芯片功耗。
- 填写实际源 commit、imageDigest、硬件/驱动范围和采集周期等验证证据。

验证后才将 `deploy/kubernetes.yaml` 中 NPU job 的 `exporter_profile` 从 `unverified` 改为
`ascend-physical-percent-mib-watts-v1`。默认 `unverified` 不生成芯片标准化指标；修改 profile
记录本身不会自动启用 Prometheus 配置。

## 5. 准备 Dashboard 和 Redis

Agent 部署前，先使 Dashboard 具备接收能力：

1. 构建包含设备模块的新 Dashboard 镜像，migration Job 与 Deployment 使用同一发布版本。
2. 配置 Dashboard PostgreSQL Secret，执行 `ModelDashBoard/deploy/migrate-job.yaml` 并等待成功。
   该 Job 显式迁移 metadata 和设备表；更新模板中的旧镜像地址后执行。已有同名完成 Job 时，
   按发布流程使用新 Job 名或重新创建，不能依赖重复 apply 再次迁移。
3. 更新 Dashboard，保持生产单写副本；配置 JWT、Ouroboros 身份校验、
   `METADATA_OPERATOR_SUBJECTS`、`METADATA_DATABASE_URL` 和 `METADATA_SCOPE`。
   `MODEL_METRICS_URL` 指向上面的内部查询地址。
4. 按 Redis ACL 轮换流程增加 `device-publisher`、`observer-publisher`，扩展
   `dashboard-reader` 来源权限并保留原凭据。向 `default` namespace 的
   `model-dashboard-device-redis` Secret 注入 `DEVICE_METADATA_PUBLISH_URL` 和
   `DEVICE_OBSERVER_PUBLISH_URL`。
5. 应用 Dashboard 设备 SA/RBAC、网络策略、Redis 放行与 Deployment patch。
   patch 开启 `DEVICE_OBSERVATION_ENABLED=true`，启用前保证运维鉴权配置完整。

以下命令从包含各仓库的 **ModelPilot 聚合工作区根目录** 执行；独立仓库环境需要取得对应文件：

```sh
kubectl apply -f ModelDashBoard/deploy/migrate-job.yaml
kubectl -n default wait --for=condition=complete job/model-dashboard-migrate --timeout=300s
# 迁移完成后，按 Dashboard 现有发布流程更新 Deployment 镜像和配置。
# 注意：device_observation 表迁移与旧版 metadata 表迁移是独立的。
# 如果 migrate-job 已执行但 device_sessions 表不存在，直接在 Dashboard Pod 内补迁移：
#   kubectl -n default exec deployment/svc-model-dashboard-f1cfb1 -c service -- \
#     python -m app.device_observation.database \
#     --database-url 'postgresql://ouroboros:changeme-postgres-password@ouroboros-postgres.default.svc:5432/ouroboros'
kubectl apply -f ModelDashBoard/deploy/device-observation.yaml
kubectl apply -f ModelPilotMetaRedis/deploy/device-observation-network.yaml
# Ouroboros MCP 部署的 Deployment 名称为 svc-model-dashboard-f1cfb1，容器名为 service
kubectl -n default patch deployment svc-model-dashboard-f1cfb1 \
  --patch-file ModelDashBoard/deploy/device-observation-patch.yaml
kubectl -n default rollout status deployment/svc-model-dashboard-f1cfb1
```

设备网络策略与原策略共同生效；保留 Dashboard 到 Prometheus、数据库、DNS、Ouroboros
以及 Portal 到 Dashboard 的既有网络放行。

## 6. 部署并启用设备 Agent

确认真机清单校验通过后，在 `deploy/device-agent/kubernetes.yaml` 的 ConfigMap 中设置
`INVENTORY_PROFILE_VERIFIED: "true"`，同时确认 `CLUSTER_ID`、`DASHBOARD_URL`、
`ASCEND_MODEL` 和 `DCMI_LIBRARY_PATH`。

回到 ModelMetrics 仓根目录执行：

```sh
kubectl apply -f deploy/device-agent/kubernetes.yaml
kubectl -n monitoring rollout status daemonset/model-device-agent
kubectl -n monitoring logs daemonset/model-device-agent --tail=100
```

Agent 使用 bound Pod projected token 注册 session 并上报，每 300 秒采集一次。
readiness 仅在成功接收后的 900 秒内成立；未验证 profile 时不会发送完整清单。
单份待发快照最多保留 10 分钟，驱动异常不会上报完整空清单。

Prometheus 配置更新后重新应用并重启以加载配置和规则：

```sh
kubectl apply -f deploy/kubernetes.yaml
kubectl -n monitoring rollout restart deployment/model-metrics
kubectl -n monitoring rollout status deployment/model-metrics
```

Agent ConfigMap 更新后也需要重启 DaemonSet，才能重新读取环境变量：

```sh
kubectl -n monitoring rollout restart daemonset/model-device-agent
kubectl -n monitoring rollout status daemonset/model-device-agent
```

## 7. 更新 Portal 并验收

部署包含设备页的新 Portal 镜像，以运维身份访问 `/analytics/devices`：

- Prometheus targets 正常，节点与芯片指标可查询；节点 CPU rate 需要累积采样。
- 设备数量、物理 ID 与真机一致；页面展示节点 CPU/内存、芯片利用率/HBM/温度/功耗。
- 设备清单与 Redis 同步状态可见；缺失、过期和采集失败有明确状态，普通用户被拒绝访问。
- 清单接收时间超过 900 秒标 stale；指标 sampleAt 是规则评估时间，硬件采样新鲜度仍标待验证。

可用本地端口转发查看 Prometheus targets 和查询结果：

```sh
kubectl -n monitoring port-forward service/model-metrics 9090:9090
```

浏览器访问 `http://127.0.0.1:9090/targets`。如集群 CNI 对端口转发有限制，使用允许访问
ModelMetrics 的 Dashboard 网络路径验证。

## 检查、当前状态与回滚

仓内检查：

```sh
cd agent
go test ./...
cd ..
python -m pytest tests
python scripts/sync_device_rules.py --check
promtool check rules rules/devices.yml
promtool test rules rules/devices.test.yml
```

截至 2026-10-10，代码与本地设备闭环已验证；新增镜像在线构建、目标集群部署、真实驱动 ABI
与 PostgreSQL 实库验收已完成。本地 fixture 不替代真机与 Kubernetes bound-token 验收。

回滚先停止 Agent 和设备特定工作负载，再关闭 Dashboard `DEVICE_OBSERVATION_ENABLED`，保留
设备事实表与 Prometheus PVC；单独 Redis 故障无需停止设备接收。保留原 ModelFlow Router
指标采集和分析看板。

聚合工作区的详细验收记录与回滚说明：`docs/runbook/device-observation-acceptance.md`。
实集群部署踩坑记录见 `deploy/TROUBLESHOOTING.md`。
