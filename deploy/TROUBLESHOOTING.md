# 实集群部署踩坑记录（k3s + 910B4 双节点）

以下问题均在 2026-10-10 ~ 2026-10-12 的真实集群部署中发现并修复，记入文档以防复发。

集群环境：k3s 单控制面 + 1 worker，两个 NPU 节点（`devserver-bms-551fff33-0` / `paas-7.242.108.205`），
910B4 芯片 × 8/节点，Dashboard 通过 Ouroboros MCP 部署。

---

## 1. exporter_profile 被 kubectl apply 覆盖

**现象**：Prometheus 能 scrape 到 NPU Exporter（`up=1`），原始指标 `npu_chip_info_utilization` 有 16 条数据，但录制规则 `modelpilot_accelerator_core_utilization_ratio` 产出 0 条。

**根因**：用 `sed` 或 `kubectl patch` 修改 ConfigMap 中的 `exporter_profile` 后，再执行 `kubectl apply -f deploy/kubernetes.yaml` 会用源文件中的 `replacement: unverified` 覆盖回去。

**修复**：必须在 `deploy/kubernetes.yaml` 源文件中将 `replacement: unverified` 改为 `replacement: ascend-physical-percent-mib-watts-v1`，再 `kubectl apply`。

---

## 2. device-rules.yml 中 vdie_id="" 过滤导致 910B4 芯片指标为空

**现象**：`exporter_profile` 已正确，原始指标有数据，录制规则仍产出 0 条。

**根因**：910B4 硬件上 NPU Exporter 的 `vdie_id` 标签始终非空（如 `6E03EE6C-80A04CD9-...`），但 `device-rules.yml` 中所有芯片级规则都过滤 `vdie_id=""`，匹配零条时间序列。

**修复**：从 `rules/devices.yml` 和 `deploy/kubernetes.yaml` 的 `device-rules.yml` 中移除所有 `,vdie_id=""` 过滤条件。

---

## 3. Device Agent CrashLoopBackOff：INVENTORY_PROFILE_VERIFIED=false

**现象**：Agent Pod 日志只有一行 `Verify DCMI physical IDs against exporter IDs on this driver before enabling the inventory profile` 后退出。

**根因**：ConfigMap 中 `INVENTORY_PROFILE_VERIFIED` 默认为 `"false"`，这是一个安全开关。`main.go` 中 `log.Fatal` 直接退出。

**修复**：
```sh
kubectl -n monitoring patch configmap model-device-agent-config \
  --type merge -p '{"data":{"INVENTORY_PROFILE_VERIFIED":"true"}}'
kubectl -n monitoring rollout restart daemonset/model-device-agent
```

---

## 4. Device Agent DNS 解析失败：DASHBOARD_URL Service 名称

**现象**：Agent Pod Running 但日志报 `dial tcp: lookup model-dashboard.default.svc.cluster.local on 10.43.0.10:53: no such host`。

**根因**：ConfigMap 中 `DASHBOARD_URL` 配置为 `http://model-dashboard.default.svc.cluster.local:8000`，但 Dashboard 通过 Ouroboros MCP 部署，Service 名称为 `svc-model-dashboard-f1cfb1-svc`，不存在名为 `model-dashboard` 的 Service。

**修复**：
```sh
kubectl -n monitoring patch configmap model-device-agent-config \
  --type merge -p '{"data":{"DASHBOARD_URL":"http://svc-model-dashboard-f1cfb1-svc.default.svc.cluster.local:8000"}}'
kubectl -n monitoring rollout restart daemonset/model-device-agent
```

---

## 5. Dashboard "Device metadata database unavailable"：device_observation 表不存在

**现象**：Dashboard Pod 日志报 `Device metadata database unavailable`，PostgreSQL 连接正常（`psycopg.connect` 成功）。

**根因**：`DeviceObservationService._run()` 的 `except Exception` 捕获所有错误并输出笼统消息。实际原因是 `device_sessions`、`device_assets`、`device_asset_history` 表不存在——旧版迁移只创建了 `metadata_current/history/sync` 表，`device_observation.database.migrate()` 需要单独执行。

**修复**：
```sh
kubectl -n default exec deployment/svc-model-dashboard-f1cfb1 -c service -- \
  python -m app.device_observation.database \
  --database-url 'postgresql://ouroboros:changeme-postgres-password@ouroboros-postgres.default.svc:5432/ouroboros'
```

---

## 6. Dashboard NetworkPolicy ingress 阻止 Portal 代理

**现象**：Dashboard API 返回 200（内部测试正常），但通过 Portal 前端访问 `/modeldashboard/api/v1/devices` 失败，显示"设备观测暂不可用"。

**根因**：`device-observation.yaml` 的 NetworkPolicy `policyTypes: [Ingress, Egress]` 中 ingress 只放行了 monitoring namespace 的 Device Agent，没有放行 default namespace 的 Portal Pod。Portal nginx 代理 `/modeldashboard/` 到 Dashboard:8000 时被 NetworkPolicy 拒绝。

**修复**：在 `device-observation.yaml` 的 ingress 中增加 default namespace 的放行规则：
```yaml
- from:
    - namespaceSelector:
        matchLabels: {kubernetes.io/metadata.name: default}
  ports: [{protocol: TCP, port: 8000}]
```

---

## 7. Dashboard NetworkPolicy egress 缺少 PostgreSQL/DNS/Prometheus/Ouroboros API

**现象**：Dashboard Pod 日志报 `Device metadata database unavailable`（PostgreSQL 连接被拒）。

**根因**：`device-observation.yaml` 的 egress 只允许 K8s API(443/6443) 和 Redis(6379)，缺少 PostgreSQL(5432)、Prometheus(9090)、DNS(53)、Ouroboros API(9174)。

**修复**：在 `device-observation.yaml` 的 egress 中增加所有缺失规则（见 `deploy-all.sh` Step 6）。

---

## 8. k3s kube-proxy DNAT 导致 NetworkPolicy ipBlock 失效

**现象**：NetworkPolicy egress 配置 `ipBlock: {cidr: 10.43.0.1/32}` 允许访问 K8s API(443)，但 Agent 仍无法连接 K8s API。

**根因**：k3s 的 kube-proxy 在 PREROUTING 链做 DNAT，将 ClusterIP `10.43.0.1:443` 重写为节点 IP:6443。NetworkPolicy 在 FORWARD 链评估 egress 时看到的是 DNAT 后的目标地址，`10.43.0.1/32` 不匹配。

**修复**：egress 规则改为 `ipBlock: {cidr: 0.0.0.0/0}` + `ports: [443, 6443]`。

---

## 9. NPU Exporter 第二节点 hostPath 路径不一致

**现象**：NPU Exporter DaemonSet 在第一节点 Running，第二节点卡在 `Init:0/1` 或 `ImageInspectError`。

**根因**：两个节点容器运行时不同——devserver 用 k3s（containerd 在 `/run/k3s/containerd`），paas 用标准 containerd（在 `/run/containerd`）。DaemonSet 的 `runtime` volume 用 `type: Directory` 强校验路径，第二节点没有该路径就挂载失败。此外 patch volume 会触发滚动更新，如果镜像还是占位符 `REGISTRY/modelpilot/npu-exporter:VERIFIED_IMMUTABLE_TAG`，两个节点都拉不到镜像。

**修复**：
1. 去掉 `runtime` volume 的 `type: Directory`（改为空字符串或不指定）
2. 用 `kubectl set image daemonset/npu-exporter npu-exporter=<实际镜像>` 替换占位符
3. 同时 patch init container 的镜像

---

## 10. Dashboard 容器内 httpx 走代理导致连接被拒

**现象**：在 Dashboard Pod 内用 `httpx.get('http://127.0.0.1:8000/...')` 或 `httpx.get('http://model-metrics...:9090/...')` 报 `ConnectError: [Errno 111] Connection refused`。

**根因**：Dashboard 镜像设置了 `HTTP_PROXY`/`HTTPS_PROXY` 环境变量，httpx 默认读取这些环境变量走代理。代理不转发 localhost 和集群内部地址。

**修复**：用 `httpx.Client(trust_env=False)` 或 `export NO_PROXY='*'` 禁用代理。排查时用 Dashboard Pod 内的 Python（`httpx.Client(trust_env=False)`）而非 curl（镜像内未安装）。

---

## 11. Ouroboros MCP 部署的 Dashboard 资源名称

**现象**：`kubectl patch deployment model-dashboard` 报 `deployments.apps "model-dashboard" not found`。

**根因**：Ouroboros MCP 部署的 Dashboard Deployment 名称为 `svc-model-dashboard-f1cfb1`（自动生成），Service 名称为 `svc-model-dashboard-f1cfb1-svc`，容器名为 `service`（不是 `model-dashboard`）。patch 文件和部署脚本中的 deployment/container 名称必须与此一致。

**修复**：更新 `device-observation-patch.yaml` 中的 deployment 名称为 `svc-model-dashboard-f1cfb1`，容器名为 `service`。
