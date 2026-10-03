# island-venues-notifier

Sends the confirmation e-mail when someone requests a venue on **Island
Venues**, the demo app for the DevFest 2026 talk *"The Era of Agentic Developer
Platforms"*: book event venues across Mauritius.

`island-venues-api` publishes a `booking-created` event to Pub/Sub. Eventarc
pushes each event to this service, which e-mails the guest through SendGrid:

```
Subject: Your Island Venues booking request

Venue: Le Morne Beach Pavilion
Date: 2026-12-12
Guests: 80
Booking ID: bk_123
```

## How it works

`POST /` receives an Eventarc Pub/Sub push:

```json
{"message": {"data": "<base64 booking-created JSON>", "messageId": "..."}, "subscription": "..."}
```

The status code is the acknowledgement:

| Outcome | Status | Pub/Sub |
|---|---|---|
| E-mail sent | `200 {"status":"sent"}` | acknowledged |
| Same `messageId` already sent by this instance | `200 {"status":"duplicate"}` | acknowledged, nothing re-sent |
| Malformed push or event (bad JSON/base64, missing or oversized `messageId`, missing field, invalid e-mail, bad date, guests < 1) | `400` (`413` if over 64 KiB) | not fixable by retrying |
| Same `messageId` is being sent right now | `503` | redelivered later |
| SendGrid failed or timed out (10 s) | `502` | redelivered |

Duplicates are filtered by `messageId` in a bounded in-memory set (the last
10,000 delivered ids per instance). A message id is only remembered once its
e-mail went out, so a failed send is retried rather than swallowed. The set is
per instance and lost on restart: it removes the common redeliveries, it does
not make delivery exactly-once.

Pub/Sub push retries **any** non-2xx answer, 4xx included. Configure a
dead-letter topic on the Eventarc subscription so a malformed event stops
after a few attempts instead of being retried until it expires.

`GET /healthz` returns `ok`.

The service does not authenticate pushes itself: whoever can call `POST /`
can make it e-mail any address with the fixed template. Keep the Cloud Run
service private (IAM `run.invoker` granted only to the Eventarc trigger's
service account) and its ingress internal.

### Observability

All of it lives in [`internal/telemetry`](internal/telemetry):

- **Logs**: `log/slog` JSON on stdout with Cloud Logging's field names
  (`severity`, `message`, `time`). Every line written during a request carries
  `logging.googleapis.com/trace` (`projects/<project>/traces/<traceId>`),
  `logging.googleapis.com/spanId` and `logging.googleapis.com/trace_sampled`, so
  Cloud Logging shows it under the request's trace.
- **Traces**: OpenTelemetry with an `otelhttp` server span on every route and a
  client span for the SendGrid call, exported to Cloud Trace when
  `GOOGLE_CLOUD_PROJECT` is set. Both W3C `traceparent` and Google's
  `X-Cloud-Trace-Context` are accepted, so the request joins the trace Cloud
  Run already started. No trace headers are sent to SendGrid.
- **Profiles**: Cloud Profiler starts when `GOOGLE_CLOUD_PROJECT` is set.
- **Shutdown**: on `SIGTERM` the server drains for 8 s, then flushes spans
  within Cloud Run's 10 s grace period.

Without `GOOGLE_CLOUD_PROJECT` nothing talks to Google APIs: tracing is a no-op
(incoming trace ids still reach the logs) and the profiler stays off.

## Run locally

Requires Go 1.26 or later.

```sh
go test ./...
go run main.go            # listens on :8080, logs e-mails instead of sending
```

Send it an event:

```sh
DATA=$(printf '%s' '{"bookingId":"bk_1","venueId":"v1","venueName":"Chamarel Hilltop Lodge","date":"2026-12-24","guests":40,"owner":"guest@example.com","contactEmail":"guest@example.com","createdAt":"2026-10-03T08:00:00Z"}' | base64 | tr -d '\n')
curl -s -d "{\"message\":{\"data\":\"$DATA\",\"messageId\":\"1\"},\"subscription\":\"local\"}" localhost:8080/
```

Set `SENDGRID_API_KEY` and `SENDER_EMAIL` to send real e-mails.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | HTTP listen port (set by Cloud Run). |
| `SENDGRID_API_KEY` | *(unset)* | SendGrid API key with Mail Send permission. With `SENDER_EMAIL`, enables real sending; otherwise e-mails are only logged. Surrounding whitespace, such as a trailing newline from Secret Manager, is ignored. |
| `SENDER_EMAIL` | *(unset)* | Verified SendGrid sender address used as `From`. |
| `GOOGLE_CLOUD_PROJECT` | *(unset)* | Enables Cloud Trace export, Cloud Profiler and the trace field in logs. Cloud Run does not set it; the Infrastream engine injects it on every deployment. |
| `SERVICE_NAME` | `island-venues-notifier` | Service name in traces and profiles. |
| `SERVICE_VERSION` | `RELEASE_VERSION`, else `INFRASTREAM_APP_VERSION`, else empty | Version in traces and profiles. The engine-built image sets `RELEASE_VERSION` and the engine sets `INFRASTREAM_APP_VERSION`. |
| `LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN` or `ERROR`. |

## Deployment

Deployed by [Infrastream](https://infrastream.io) into the managed tenant
`island-venues` (region `us-central1`). The manifests live in
[`a-manraj-infrastream/infrastream-organization-manifests-ab204170`](https://github.com/a-manraj-infrastream/infrastream-organization-manifests-ab204170):
the `island-venues-notifier` Application runs on Cloud Run with a sidecar
mesh, behind an Eventarc trigger on the `booking-created` topic.

The engine builds this repository as a Go binary: CI runs `go test ./...`
then `go build main.go` and generates the container image, so there is no
Dockerfile here and `package main` must stay in `main.go`. The SendGrid API
(`api.sendgrid.com`) is the project's only allowed egress.

## License

[Apache 2.0](LICENSE)
