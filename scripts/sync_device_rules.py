"""Keep the deploy ConfigMap's rules identical to the independently testable source."""
import argparse
from pathlib import Path

root = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser()
parser.add_argument("--check", action="store_true")
args = parser.parse_args()
path = root / "deploy/kubernetes.yaml"
text = path.read_text(encoding="utf-8-sig")
rules = (root / "rules/devices.yml").read_text(encoding="utf-8").rstrip()
embedded = "  device-rules.yml: |\n" + "\n".join("    " + line for line in rules.splitlines()) + "\n"
marker = "---\napiVersion: v1\nkind: PersistentVolumeClaim"
if "  device-rules.yml: |\n" in text:
    start = text.index("  device-rules.yml: |\n")
    end = text.index(marker, start)
    updated = text[:start] + embedded + text[end:]
else:
    updated = text.replace(marker, embedded + marker, 1)
if args.check:
    if updated != text:
        raise SystemExit("Device rules ConfigMap has drifted; run scripts/sync_device_rules.py")
else:
    path.write_text(updated, encoding="utf-8", newline="\n")
