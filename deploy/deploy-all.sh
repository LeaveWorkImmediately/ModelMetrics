#!/bin/bash
# =============================================================================
# ModelMetrics + Device Observation 完整部署脚本
# 在服务器上执行，需先 cd 到包含 ModelMetrics 和 ModelDashBoard 的工作区根目录
# 前置条件：
#   - Device Agent 镜像已构建 (tag=20261010-144226)
#   - Dashboard 镜像已构建并更新 (tag=20261010-142500，已通过 MCP 滚动更新)
#   - 两台 NPU 节点：devserver-bms-551fff33-0 / paas-7.242.108.205
# =============================================================================

set -e

echo "=== 0. 确认镜像和节点 ==="
echo "Device Agent: 7.242.106.192:14480/modelpilot/model-device-agent:20261010-144226"
echo "Dashboard:    7.242.106.192:14480/modelpilot/model-dashboard:20261010-142500 (已部署)"
echo "NPU Exporter: swr.cn-south-1.myhuaweicloud.com/ascendhub/npu-exporter:v26.1.0-openeuler24.03"
echo ""

# =============================================================================
# 1. 部署 Prometheus 主服务 (monitoring namespace)
#    包含 namespace、ConfigMap(含 device-rules.yml)、PVC、Deployment、Service、NetworkPolicy
#    deploy/kubernetes.yaml 中 exporter_profile 已设为 ascend-physical-percent-mib-watts-v1
#    device-rules.yml 中的 vdie_id="" 过滤已移除（910B4 硬件 vdie_id 始终非空）
# =============================================================================
echo "=== 1. 部署 Prometheus 主服务 (monitoring namespace) ==="
kubectl apply -f ModelMetrics/deploy/kubernetes.yaml
kubectl -n monitoring get pvc model-metrics-data
kubectl -n monitoring rollout status deployment/model-metrics
echo ""

# =============================================================================
# 2. 给节点打 label (设备观察)
#    两个 NPU 节点都必须打此 label，所有 DaemonSet 都使用此 nodeSelector
# =============================================================================
echo "=== 2. 给节点打 label (设备观察) ==="
kubectl label node devserver-bms-551fff33-0 modelpilot.io/device-observation=true --overwrite
kubectl label node paas-7.242.108.205 modelpilot.io/device-observation=true --overwrite
echo ""

# =============================================================================
# 3. 部署 node_exporter (DaemonSet, monitoring namespace)
#    注意：node_exporter 使用 hostNetwork，监听宿主 9100 端口
#    Prometheus NetworkPolicy 已放行 0.0.0.0/0:9100（按需收窄到实际节点 IP）
# =============================================================================
echo "=== 3. 部署 node_exporter ==="
kubectl apply -f ModelMetrics/deploy/node-exporter/kubernetes.yaml
kubectl -n monitoring rollout status daemonset/model-node-exporter
echo ""

# =============================================================================
# 4. 部署 Ascend NPU Exporter (DaemonSet, npu-exporter namespace)
#    重要：deploy/exporters/ascend/kubernetes.yaml 中的镜像是占位符
#          需要替换为实际镜像后部署
#    注意：两个节点容器运行时可能不同：
#      - devserver (k3s): containerd 在 /run/k3s/containerd
#      - paas (标准): containerd 在 /run/containerd
#    如 hostPath type: Directory 导致第二个节点 Pod 失败，去掉 type 字段或改为空字符串
# =============================================================================
echo "=== 4. 部署 Ascend NPU Exporter ==="
# 镜像替换（如果 YAML 中还是占位符）
sed -i 's|REGISTRY/modelpilot/npu-exporter:VERIFIED_IMMUTABLE_TAG|swr.cn-south-1.myhuaweicloud.com/ascendhub/npu-exporter:v26.1.0-openeuler24.03|g' \
  ModelMetrics/deploy/exporters/ascend/kubernetes.yaml 2>/dev/null || true

kubectl apply -f ModelMetrics/deploy/exporters/ascend/kubernetes.yaml
# NPU Exporter DaemonSet 可能需要手动修复第二节点 volume，这里只等待第一节点
echo "等待 NPU Exporter Pod 启动..."
sleep 15
kubectl -n npu-exporter get pods -o wide
# 如果有 Pod 不 Ready，手动 kubectl edit daemonset/npu-exporter 修复 volume 路径
echo ""

# =============================================================================
# 5. 执行 Dashboard device_observation 数据库迁移
#    重要：旧版 migrate-job 只迁移 metadata_current/history/sync 表
#          device_observation 需要单独迁移 device_sessions/device_assets/device_asset_history 表
#    如果 migrate-job 的 Secret 不存在，可以直接在 Dashboard Pod 内执行迁移：
#      kubectl -n default exec deployment/svc-model-dashboard-f1cfb1 -c service -- \
#        python -m app.device_observation.database \
#        --database-url 'postgresql://ouroboros:changeme-postgres-password@ouroboros-postgres.default.svc:5432/ouroboros'
# =============================================================================
echo "=== 5. 执行 device_observation 数据库迁移 ==="
# 方式 1：用 migrate-job（需要 model-dashboard-metadata-db Secret）
# kubectl apply -f ModelDashBoard/deploy/migrate-job.yaml
# kubectl -n default wait --for=condition=complete job/model-dashboard-migrate --timeout=300s

# 方式 2：直接在 Dashboard Pod 内执行（更可靠，绕过 Secret 依赖）
kubectl -n default exec deployment/svc-model-dashboard-f1cfb1 -c service -- \
  python -m app.device_observation.database \
  --database-url 'postgresql://ouroboros:changeme-postgres-password@ouroboros-postgres.default.svc:5432/ouroboros'
echo "迁移完成"
echo ""

# =============================================================================
# 6. 应用 Dashboard 设备观察 SA/RBAC/NetworkPolicy
#    device-observation.yaml 包含：
#      - ServiceAccount (device-observation)