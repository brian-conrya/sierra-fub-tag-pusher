# sierra-fub-tag-pusher

Syncs tag additions from Sierra Interactive to Follow Up Boss.

## Why this exists

Follow Up Boss's built-in Sierra Interactive integration syncs tags, but
the delay between a tag being added in Sierra and it appearing in FUB can
be long enough that downstream automations (drip campaigns, smart lists,
agent routing) fire on stale state — or not at all in time to matter.

This service is a thin webhook bridge that listens for Sierra's
`LeadTagAdded` event in real time and pushes the tag to FUB within
seconds. Tag updates land in FUB fast enough to be useful for time-
sensitive workflows; the official integration continues to handle
everything else.

## Deploy

[![Run on Google Cloud](https://deploy.cloud.run/button.svg)](https://deploy.cloud.run)

Clicking the button launches Cloud Shell, prompts for your Sierra Interactive
and Follow Up Boss API keys, deploys the service to Cloud Run, writes the
keys to Secret Manager, and registers the webhook with Sierra.

The whole flow takes about 2 minutes.

## Re-deploying

Click the same button again. Cloud Run creates a new revision and shifts
traffic to it with no dropped requests, so re-deploys are seamless from the
caller's perspective. The webhook URL is stable across revisions, so Sierra
needs no reconfiguration; the deploy script detects the existing
subscription and skips re-registering it.

If you'd rather deploy from a local clone:

```sh
gcloud run deploy sierra-fub-tag-pusher --source . --region <your-region>
```

The Cloud Run service URL stays the same as long as you keep the service
name and region.

## What you need

- A Google Cloud project (Cloud Run + Secret Manager will be enabled
  automatically).
- A Sierra Interactive API key.
- A Follow Up Boss API key.

## Development

The project builds and tests with stock Go 1.26. No third-party runtime
dependencies.

```sh
make test         # run tests
make test-race    # tests with race detector
make vet          # go vet
make lint         # golangci-lint (install: brew install golangci-lint)
make fmt          # gofmt -s -w .
make build        # builds ./bin/pusher
make ci           # everything CI runs: tidy, fmt, vet, lint, race tests, build
```

GitHub Actions runs `make ci` equivalent on every push / pull request (see
[.github/workflows/ci.yml](.github/workflows/ci.yml)).

## Configuration

| Variable | Required | Default |
|---|---|---|
| `SIERRA_API_KEY` | yes | — |
| `FUB_API_KEY` | yes | — |
| `PORT` | no | `8080` (Cloud Run injects this) |
| `LOG_LEVEL` | no | `info` |

Both API base URLs (Sierra Interactive and Follow Up Boss) are compiled-in
constants in [internal/sierra](internal/sierra/sierra.go) and
[internal/fub](internal/fub/fub.go). They have never changed; they live in
code so they can't be misconfigured at deploy time.

## Build & container image

The repo has no `Dockerfile`. Cloud Run (and the Cloud Run Button)
auto-detects Go and builds the container with
[Google Cloud Buildpacks](https://cloud.google.com/docs/buildpacks/overview),
which means the runtime base image is patched by Google rather than pinned
here — no manual `golang:1.x` or `distroless:nonroot` upgrades to track.

## Project layout

```
cmd/pusher/                 entry point
internal/handler/           HTTP routes, payload validation, orchestration
internal/sierra/            Sierra API client + webhook payload types
internal/fub/               Follow Up Boss API client
internal/retry/             HTTP retry with Retry-After + capped backoff
internal/logging/           slog setup
testdata/sierra/            captured Sierra payload fixtures used in tests
deploy/postcreate.sh        Cloud Run Button postcreate hook (idempotent)
```
