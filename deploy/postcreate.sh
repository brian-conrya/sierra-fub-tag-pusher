#!/usr/bin/env bash
# Cloud Run Button postcreate hook.
#
# Runs after Cloud Run creates (or re-creates) the service. Idempotent: safe
# to run on the first deploy and on every subsequent re-deploy.
#
# Responsibilities:
#   1. Push SIERRA_API_KEY and FUB_API_KEY into Secret Manager (versioned).
#   2. Re-deploy the service to read those keys from Secret Manager
#      (plaintext envs are then removed from the service spec).
#   3. Register the webhook subscription with Sierra Interactive, but skip
#      if a subscription already exists for this service's URL.
#
# Expected environment (injected by Cloud Run Button):
#   GOOGLE_CLOUD_PROJECT, GOOGLE_CLOUD_REGION, K_SERVICE
#   SIERRA_API_KEY, FUB_API_KEY

set -euo pipefail

: "${GOOGLE_CLOUD_PROJECT:?must be set by Cloud Run Button}"
: "${K_SERVICE:?must be set by Cloud Run Button}"
: "${SIERRA_API_KEY:?must be supplied by the deployer}"
: "${FUB_API_KEY:?must be supplied by the deployer}"

REGION="${GOOGLE_CLOUD_REGION:-${CLOUDSDK_RUN_REGION:-us-central1}}"
SIERRA_BASE_URL="https://api.sierrainteractivedev.com"

echo "→ Enabling required APIs"
gcloud services enable \
  secretmanager.googleapis.com \
  run.googleapis.com \
  --project "${GOOGLE_CLOUD_PROJECT}" >/dev/null

echo "→ Writing secrets to Secret Manager"
upsert_secret() {
  local name="$1"
  local value="$2"
  if gcloud secrets describe "${name}" --project "${GOOGLE_CLOUD_PROJECT}" >/dev/null 2>&1; then
    printf '%s' "${value}" | gcloud secrets versions add "${name}" \
      --data-file=- --project "${GOOGLE_CLOUD_PROJECT}" >/dev/null
  else
    printf '%s' "${value}" | gcloud secrets create "${name}" \
      --replication-policy=automatic --data-file=- \
      --project "${GOOGLE_CLOUD_PROJECT}" >/dev/null
  fi
}

upsert_secret "${K_SERVICE}-sierra-api-key" "${SIERRA_API_KEY}"
upsert_secret "${K_SERVICE}-fub-api-key" "${FUB_API_KEY}"

echo "→ Granting Cloud Run runtime SA access to the secrets"
RUNTIME_SA="$(gcloud run services describe "${K_SERVICE}" \
  --region "${REGION}" --project "${GOOGLE_CLOUD_PROJECT}" \
  --format='value(spec.template.spec.serviceAccountName)')"
if [[ -z "${RUNTIME_SA}" ]]; then
  PROJECT_NUMBER="$(gcloud projects describe "${GOOGLE_CLOUD_PROJECT}" --format='value(projectNumber)')"
  RUNTIME_SA="${PROJECT_NUMBER}-compute@developer.gserviceaccount.com"
fi
for secret in "${K_SERVICE}-sierra-api-key" "${K_SERVICE}-fub-api-key"; do
  gcloud secrets add-iam-policy-binding "${secret}" \
    --member="serviceAccount:${RUNTIME_SA}" \
    --role="roles/secretmanager.secretAccessor" \
    --project "${GOOGLE_CLOUD_PROJECT}" >/dev/null
done

echo "→ Re-deploying Cloud Run service with secret references"
gcloud run services update "${K_SERVICE}" \
  --region "${REGION}" \
  --project "${GOOGLE_CLOUD_PROJECT}" \
  --remove-env-vars=SIERRA_API_KEY,FUB_API_KEY \
  --set-secrets="SIERRA_API_KEY=${K_SERVICE}-sierra-api-key:latest,FUB_API_KEY=${K_SERVICE}-fub-api-key:latest" \
  >/dev/null

SERVICE_URL="$(gcloud run services describe "${K_SERVICE}" \
  --region "${REGION}" --project "${GOOGLE_CLOUD_PROJECT}" \
  --format='value(status.url)')"
WEBHOOK_URL="${SERVICE_URL}/webhook"

echo "→ Checking Sierra for an existing webhook subscription"
EXISTING="$(
  curl -fsS "${SIERRA_BASE_URL}/webhook" \
    -H "Sierra-ApiKey: ${SIERRA_API_KEY}" \
    -H 'Accept: application/json'
)"
if grep -qF "\"${WEBHOOK_URL}\"" <<<"${EXISTING}"; then
  echo "  ✓ A subscription for ${WEBHOOK_URL} already exists. Skipping registration."
else
  echo "→ Registering webhook with Sierra Interactive"
  RESPONSE="$(
    curl -fsS -X POST "${SIERRA_BASE_URL}/webhook" \
      -H "Sierra-ApiKey: ${SIERRA_API_KEY}" \
      -H 'Content-Type: application/json' \
      -d "{\"eventTypes\":[\"LeadTagAdded\"],\"url\":\"${WEBHOOK_URL}\"}"
  )"
  echo "Sierra response: ${RESPONSE}"
  if ! grep -q '"success":[[:space:]]*true' <<<"${RESPONSE}"; then
    echo "Sierra webhook registration FAILED. Check your SIERRA_API_KEY." >&2
    exit 1
  fi
fi

cat <<EOF

✓ Sierra → FUB tag pusher is live.

  Service URL : ${SERVICE_URL}
  Webhook URL : ${WEBHOOK_URL}
  Logs        : https://console.cloud.google.com/run/detail/${REGION}/${K_SERVICE}/logs?project=${GOOGLE_CLOUD_PROJECT}

To deploy a new version: click the Run on Google Cloud button again. Cloud
Run rolls out a new revision with zero downtime and the webhook URL stays
the same, so Sierra needs no reconfiguration.

EOF
