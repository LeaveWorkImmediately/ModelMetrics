#!/bin/bash
# ModelMetrics 完整部署脚本
# 在服务器上执行，需先 cd 到包含 ModelMetrics 和 ModelDashBoard 的工作区根目录
# 前置条件：Device Agent 镜像构建完成 (tag=20261010-144226)
#           Dashboard 镜像已构建并更新 (tag=20261010-142500，已通过 MCP 滚动更新)

set -e

echo "=== 0. 确认镜像存在 ==="
# Device Agent 镜像
echo "model-device-agent:20261010-144226"
echo "model-dashboard:20261010-142500 (已部署)"
echo ""

echo "=== 1. 部署 Prometheus 主服务 (monitoring namespace) ==="
kubectl apply -f ModelMetrics/deploy/kubernetes.yaml
kubectl -n monitoring get pvc model-metrics-data
kubectl -n monitoring rollout status deployment/model-metrics
echo ""

echo "=== 2. 给节点打 label (设备观察) ==="
# 两个 NPU 节点都需要打 label
kubectl label node devserver-bms-551fff33-0 modelpilot.io/device-observation=true --overwrite
kubectl label node paas-7.242.108.205 modelpilot.io/device-observation=true --overwrite
echo ""

echo "=== 3. 部署 node_exporter (DaemonSet, monitoring namespace) ==="
kubectl apply -f ModelMetrics/deploy/node-exporter/kubernetes.yaml
kubectl -n monitoring rollout status daemonset/model-node-exporter
echo ""

echo "=== 4. 部署 Ascend NPU Exporter (DaemonSet, npu-exporter namespace) ==="
# 注意：NPU Exporter 镜像是安装输入，需替换为已验证镜像
# 当前 deploy/exporters/ascend/kubernetes.yaml 中是占位符 REGISTRY/modelpilot/npu-exporter:VERIFIED_IMMUTABLE_TAG
# 如果有已验证的 NPU Exporter 镜像，先替换 YAML 中的 image 再 apply
# kubectl apply -f ModelMetrics/deploy/exporters/ascend/kubernetes.yaml
# kubectl -n npu-exporter rollout status daemonset/npu-exporter
echo "SKIP: NPU Exporter 镜像是安装输入，需要手动替换镜像后部署"
echo ""

echo "=== 5. 应用 Dashboard 设备观察 SA/RBAC/NetworkPolicy ==="
kubectl apply -f ModelDashBoard/deploy/device-observation.yaml
echo ""

echo "=== 6. Patch Dashboard Deployment (开启 DEVICE_OBSERVATION_ENABLED) ==="
# Ouroboros MCP 部署的 Dashboard deployment 名称为 svc-model-dashboard-f1cfb1
kubectl -n default patch deployment svc-model-dashboard-f1cfb1 \
  --patch-file ModelDashBoard/deploy/device-observation-patch.yaml
kubectl -n default rollout status deployment/svc-model-dashboard-f1cfb1
echo ""

echo "=== 7. 部署设备 Agent (DaemonSet, monitoring namespace) ==="
kubectl apply -f ModelMetrics/deploy/device-agent/kubernetes.yaml
kubectl -n monitoring rollout status daemonset/model-device-agent
echo ""

echo "=== 8. 重启 Prometheus 加载新配置和规则 ==="
kubectl apply -f ModelMetrics/deploy/kubernetes.yaml
kubectl -n monitoring rollout restart deployment/model-metrics
kubectl -n monitoring rollout status deployment/model-metrics
echo ""

echo "=== 9. 验证 ==="
echo "--- Prometheus targets ---"
kubectl -n monitoring port-forward service/model-metrics 9090:9090 &
PF_PID=$!
sleep 3
curl -s http://127.0.0.1:9090/api/v1/targets | python3 -m json.tool 2>/dev/null | head -50 || echo "port-forward 可能受限，用 kubectl logs 验证"
kill $PF_PID 2>/dev/null || true

echo ""
echo "--- Pod 状态 ---"
kubectl -n monitoring get pods -o wide
kubectl -n npu-exporter get pods -o wide 2>/dev/null || echo "npu-exporter namespace 未部署"

echo ""
echo "--- Dashboard 日志 (检查设备观察) ---"
kubectl -n default logs deployment/model-dashboard --tail=20

echo ""
echo "--- Device Agent 日志 ---"
kubectl -n monitoring logs daemonset/model-device-agent --tail=20

echo ""
echo "=== 部署完成 ==="
echo "Dashboard: http://7.242.108.205:31504/"
echo "Prometheus: http://model-metrics.monitoring.svc.cluster.local:9090 (集群内)"
echo "设备页面: http://7.242.108.205:31504/analytics/devices"
