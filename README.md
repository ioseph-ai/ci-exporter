# ci-exporter

A small Prometheus exporter for [GitLab CI](https://docs.gitlab.com/ee/api/jobs.html):
it polls the jobs API for **running** and **pending** jobs of a fixed project
list and exposes them as gauges, so alerting rules can page on:

- a **hung job** burning shared-runner minutes (`gitlab_ci_job_age_seconds`),
- a **stuck queue** — pending jobs piling up because a runner died
  (`gitlab_ci_pending_jobs`).

Single static binary, distroless image, no dependencies beyond the Prometheus
client library. Ships with a GitHub Actions workflow that lint/tests/builds and
publishes the image to GHCR on every `v*` tag.

## Metrics

| Metric | Labels | Meaning |
|---|---|---|
| `gitlab_ci_job_age_seconds` | project, job_name, status | Age of each currently **running** job (since `started_at`, falling back to `created_at`) |
| `gitlab_ci_pending_jobs` | project | Count of **pending** jobs per project |
| `gitlab_ci_scrape_errors_total` | project | Poll cycles that failed for that project |
| `gitlab_ci_scrape_duration_seconds` | — | Duration of the last full poll cycle |
| `gitlab_ci_last_scrape_timestamp` | — | Unix time of the last poll attempt (freshness watchdog) |

Plus the standard Go runtime collectors.

Polling: `GET /api/v4/projects/{id}/jobs?scope[]=running&scope[]=pending&per_page=50`
every `SCRAPE_INTERVAL` (default 60s). A >50-result page logs a truncation
warning. A failed project poll drops that project's series for the cycle
(they go stale) and bumps `gitlab_ci_scrape_errors_total` — scrapes never
block on the GitLab API.

## Configuration (env)

| Var | Default | |
|---|---|---|
| `GITLAB_URL` | `https://gitlab.com` | Instance base URL |
| `GITLAB_TOKEN` | — (required) | PAT with `read_api` only |
| `PROJECTS` | `1001:example` | Comma list of `id:shortname` |
| `SCRAPE_INTERVAL` | `60s` | Poll interval |
| `LISTEN_ADDR` | `:8080` | Metrics listen address |

Endpoints: `/metrics`, `/healthz` (503 until the first successful poll).

## Run

```yaml
# Kubernetes example — Deployment + Service
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ci-exporter
spec:
  replicas: 1
  selector:
    matchLabels: { app: ci-exporter }
  template:
    metadata:
      labels: { app: ci-exporter }
    spec:
      containers:
        - name: exporter
          image: ghcr.io/skbki/ci-exporter:v0.1.1   # example tag
          ports:
            - name: metrics
              containerPort: 8080
          env:
            - name: GITLAB_URL
              value: https://gitlab.example.com
            - name: GITLAB_TOKEN
              valueFrom:
                secretKeyRef: { name: ci-exporter, key: token }
            - name: PROJECTS
              value: "1001:example"
```

## Development

```
go build ./... && go vet ./... && go test ./...
GITLAB_TOKEN=... go run .   # local smoke test, then curl :8080/metrics
```
