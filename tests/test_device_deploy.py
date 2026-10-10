from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[1]


def test_daemonset_machine_identity_and_host_mounts():
    resources = list(yaml.safe_load_all((ROOT / "deploy/device-agent/kubernetes.yaml").read_text()))
    daemon = next(item for item in resources if item["kind"] == "DaemonSet")
    pod = daemon["spec"]["template"]["spec"]
    assert pod["serviceAccountName"] == "model-device-agent"
    assert pod["nodeSelector"] == {"modelpilot.io/device-observation": "true"}
    token = next(volume for volume in pod["volumes"] if volume["name"] == "machine-token")
    assert token["projected"]["sources"][0]["serviceAccountToken"]["audience"] == "modeldashboard-device-observation"
    assert all(mount["readOnly"] for mount in pod["containers"][0]["volumeMounts"])
    assert "startupProbe" in pod["containers"][0] and "readinessProbe" in pod["containers"][0]
    config = next(item for item in resources if item["kind"] == "ConfigMap")
    assert config["data"]["INVENTORY_PROFILE_VERIFIED"] == "false"


def test_rules_match_deployed_config_and_scrape_is_unique():
    resources = list(yaml.safe_load_all((ROOT / "deploy/kubernetes.yaml").read_text()))
    config = next(item for item in resources if item["kind"] == "ConfigMap")["data"]
    assert config["device-rules.yml"].strip() == (ROOT / "rules/devices.yml").read_text().strip()
    prometheus = yaml.safe_load(config["prometheus.yml"])
    assert prometheus["rule_files"] == ["/etc/prometheus/device-rules.yml"]
    npu = next(job for job in prometheus["scrape_configs"] if job["job_name"] == "npu-exporter")
    assert any(rule.get("regex") == "npu-exporter;8082" for rule in npu["relabel_configs"])
    assert any(rule.get("target_label") == "exporter_profile" and rule["replacement"] == "unverified" for rule in npu["relabel_configs"])


def test_git_and_local_builds_use_same_runtime():
    for name in ("Dockerfile", "Dockerfile.git"):
        text = (ROOT / "agent" / name).read_text()
        assert "FROM python:3.12-slim-bookworm" in text
        assert "CGO_ENABLED=0 go build" in text
        assert 'ENTRYPOINT ["/usr/local/bin/model-device-agent"]' in text
    git = (ROOT / "agent/Dockerfile.git").read_text()
    for name in ("GIT_REPO_URL", "GIT_REF", "GIT_TOKEN", "CACHEBUST", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"):
        assert f"ARG {name}" in git
