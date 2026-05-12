# sierra-fub-tag-pusher

Pushes Sierra Interactive tag additions into Follow Up Boss in seconds.

## Why this exists

Follow Up Boss's built-in Sierra Interactive integration syncs tags with
enough delay that downstream FUB workflows keyed off those tags — drip
campaigns, smart lists — fire late or miss their window entirely.

This service is a webhook bridge that listens for Sierra's `LeadTagAdded`
event in real time and pushes the tag to FUB within seconds, so the tag
is in place by the time FUB's automations look for it. The official
integration continues to handle everything else.

## Deploy

[![Run on Google Cloud](https://deploy.cloud.run/button.svg)](https://deploy.cloud.run)

You'll be prompted for a Sierra Interactive API key and a Follow Up Boss
API key. The button enables the needed GCP APIs, deploys to Cloud Run,
stores both keys in Secret Manager, and registers the webhook with Sierra.

Re-deploys (click the button again, or `gcloud run deploy --source .`)
roll out new Cloud Run revisions with no dropped requests and reuse the
existing Sierra subscription.

## Development

Stock Go 1.26, no third-party runtime dependencies.

```sh
make test         # run tests
make test-race    # tests with race detector
make vet          # go vet
make lint         # golangci-lint
make fmt          # gofmt -s -w .
make build        # builds ./bin/pusher
make ci           # tidy + fmt + vet + lint + race tests + build
```

GitHub Actions runs the same on every push and PR.

## Configuration

| Variable | Required | Default |
|---|---|---|
| `SIERRA_API_KEY` | yes | — |
| `FUB_API_KEY` | yes | — |
| `PORT` | no | `8080` (Cloud Run injects this) |
| `LOG_LEVEL` | no | `info` (also accepts `debug`, `warn`, `error`) |

## Project layout

```
cmd/pusher/            entry point
internal/handler/      HTTP routes, payload validation, orchestration
internal/sierra/       Sierra API client + webhook payload types
internal/fub/          Follow Up Boss API client
internal/retry/        HTTP retry with Retry-After + capped backoff
internal/logging/      slog setup
testdata/sierra/       captured Sierra payload fixtures
deploy/postcreate.sh   Cloud Run Button postcreate hook (idempotent)
```
