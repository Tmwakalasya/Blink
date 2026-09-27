#!/usr/bin/env bash
# Builds Blink's pre-built image: Debian 12 with VS Code (code-server), git,
# pip, venv and build tools installed, so VMs are ready in seconds instead of
# installing at boot. Blink uses the newest image in the "blink" family once
# it exists; restart Blink after building. Re-run to refresh the tools.
set -euo pipefail
cd "$(dirname "$0")/.."

PROJECT=${PROJECT:-$(gcloud config get-value project 2>/dev/null)}
ZONE=${ZONE:-us-central1-a}
BUILDER=blink-image-builder-$(date +%s)
IMAGE=blink-$(date +%Y%m%d-%H%M%S)
SCRIPT=$(mktemp)
trap 'rm -f "$SCRIPT"; gcloud compute instances delete "$BUILDER" --zone "$ZONE" --project "$PROJECT" --quiet >/dev/null 2>&1 || true' EXIT

cat >"$SCRIPT" <<'BAKE'
#!/bin/bash
set -euxo pipefail
export HOME=/root DEBIAN_FRONTEND=noninteractive # startup scripts run without HOME
apt-get update
apt-get install -y git python3-pip python3-venv build-essential curl
curl -fsSL https://code-server.dev/install.sh | sh
# VS Code starts at boot on VMs flagged for it (see deploy/blink-editor.service).
id blink >/dev/null 2>&1 || useradd --create-home --shell /bin/bash blink
curl -sf -H "Metadata-Flavor: Google" \
  http://metadata.google.internal/computeMetadata/v1/instance/attributes/blink-editor-unit \
  >/etc/systemd/system/blink-editor.service
systemctl daemon-reload
systemctl enable blink-editor
# These VMs live for hours at most: skip boot-time chores they never benefit from.
systemctl disable apt-daily.timer apt-daily-upgrade.timer man-db.timer unattended-upgrades google-osconfig-agent || true
apt-get clean
echo BLINK-IMAGE-READY
BAKE

echo "Building $IMAGE in $PROJECT"
# The builder deletes itself after 30 minutes if anything hangs.
gcloud compute instances create "$BUILDER" --project "$PROJECT" --zone "$ZONE" \
  --machine-type e2-medium --image-family debian-12 --image-project debian-cloud \
  --boot-disk-size 10GB --boot-disk-type pd-balanced \
  --metadata-from-file startup-script="$SCRIPT",blink-editor-unit=deploy/blink-editor.service \
  --max-run-duration 30m --instance-termination-action DELETE >/dev/null

echo "Installing tools (a few minutes)"
for _ in $(seq 1 90); do
  log=$(gcloud compute instances get-serial-port-output "$BUILDER" --zone "$ZONE" --project "$PROJECT" 2>/dev/null || true)
  if grep -q "BLINK-IMAGE-READY" <<<"$log"; then
    break
  fi
  if grep -q 'Script "startup-script" failed' <<<"$log"; then
    echo "The install failed. The builder's serial output:" >&2
    tail -40 <<<"$log" >&2
    exit 1
  fi
  sleep 10
done
grep -q "BLINK-IMAGE-READY" <<<"$log" || { echo "The install didn't finish in 15 minutes." >&2; exit 1; }

gcloud compute instances stop "$BUILDER" --zone "$ZONE" --project "$PROJECT" --quiet >/dev/null
gcloud compute images create "$IMAGE" --project "$PROJECT" --family blink \
  --source-disk "$BUILDER" --source-disk-zone "$ZONE" >/dev/null

# Keep only the newest image; older ones would just cost storage.
for old in $(gcloud compute images list --project "$PROJECT" --no-standard-images \
  --filter="family=blink AND name!=$IMAGE" --format="value(name)"); do
  gcloud compute images delete "$old" --project "$PROJECT" --quiet >/dev/null
done

echo "Built $IMAGE. Restart Blink to use it."
