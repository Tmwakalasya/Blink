#!/usr/bin/env bash
# Deploys Blink to Cloud Run for a class. Run it from anywhere in the repo:
#
#   CLIENT_ID=123-abc.apps.googleusercontent.com ADMINS=you@example.com deploy/cloudrun.sh
#
# It creates what's missing (a service account, a bucket for Blink's state,
# the Cloud Run service) and ships the current code. Re-run it to update.
set -euo pipefail
cd "$(dirname "$0")/.."

PROJECT=${PROJECT:-$(gcloud config get-value project 2>/dev/null)}
REGION=${REGION:-us-central1}
SERVICE=${SERVICE:-blink}
BUCKET=${BUCKET:-$PROJECT-blink}
BUDGET=${BUDGET:-10}
WEEKLY_HOURS=${WEEKLY_HOURS:-4}
MAX_VMS=${MAX_VMS:-10}
SIZES=${SIZES:-small,medium,large}
MAX_TTL=${MAX_TTL:-2h}
: "${CLIENT_ID:?set CLIENT_ID to your Google OAuth client ID}"
: "${ADMINS:?set ADMINS to the emails that manage the class list}"
SA=blink-server@$PROJECT.iam.gserviceaccount.com

echo "Deploying Blink to $PROJECT in $REGION"

gcloud services enable run.googleapis.com cloudbuild.googleapis.com artifactregistry.googleapis.com \
  compute.googleapis.com --project "$PROJECT"

# Blink runs as its own service account: it can manage VMs and read network
# settings, and nothing else in the project.
if ! gcloud iam service-accounts describe "$SA" --project "$PROJECT" >/dev/null 2>&1; then
  gcloud iam service-accounts create blink-server --display-name "Blink server" --project "$PROJECT"
fi
for role in roles/compute.instanceAdmin.v1 roles/compute.networkViewer; do
  gcloud projects add-iam-policy-binding "$PROJECT" --member "serviceAccount:$SA" --role "$role" \
    --condition None --quiet >/dev/null
done

# Keys, the usage ledger and the class list live in a bucket, so they
# survive restarts and new versions.
if ! gcloud storage buckets describe "gs://$BUCKET" --project "$PROJECT" >/dev/null 2>&1; then
  gcloud storage buckets create "gs://$BUCKET" --location "$REGION" --uniform-bucket-level-access --project "$PROJECT"
fi
gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" --member "serviceAccount:$SA" \
  --role roles/storage.objectUser --quiet >/dev/null

# One instance keeps the ledger and live shells in one place. Requests may
# run an hour, the longest Cloud Run allows; terminals reconnect after that.
gcloud run deploy "$SERVICE" --source . --region "$REGION" --project "$PROJECT" \
  --service-account "$SA" --allow-unauthenticated \
  --max-instances 1 --timeout 3600 --cpu 1 --memory 512Mi --execution-environment gen2 \
  --clear-volumes --add-volume "name=state,type=cloud-storage,bucket=$BUCKET" \
  --clear-volume-mounts --add-volume-mount "volume=state,mount-path=/state" \
  --set-env-vars "^;^BLINK_PROJECT=$PROJECT;BLINK_STATE=/state;BLINK_CLIENT_ID=$CLIENT_ID;BLINK_ADMINS=$ADMINS;BLINK_BUDGET=$BUDGET;BLINK_WEEKLY_HOURS=$WEEKLY_HOURS;BLINK_MAX_VMS=$MAX_VMS;BLINK_SIZES=$SIZES;BLINK_MAX_TTL=$MAX_TTL"

NUMBER=$(gcloud projects describe "$PROJECT" --format 'value(projectNumber)')
echo
echo "Blink is at https://$SERVICE-$NUMBER.$REGION.run.app"
