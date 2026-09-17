# ci-exporter

A small Prometheus exporter for CI job states — **GitLab CI** or **GitHub
Actions**, selected with `MODE`. It polls the CI API for **running** and
**pending** jobs of a fixed project list and exposes them as gauges, so
alerting rules can page on:

- a **hung job** burning runner minutes (`gitlab_ci_job_age_seconds`),
- a **stuck queue** — pending jobs piling up because a runner died
  (`gitlab_ci_pending_jobs`).

Single static binary, distroless image, no dependencies beyond the Prometheus
client library. Ships with a GitHub Actions workflow that lint/tests/builds and
publishes the image to GHCR on every `v*` tag.

## Metric contract (mode-stable)

Metric names and `project` label semantics are IDENTICAL in both modes so
alert rules fire unchanged when the backend switches. GitLab project IDs map
to repo names as the `project` label; status strings are normalized
(GitLab `running`/`pending`, GitHub `in_progress`/`queued`).

| Metric | Labels | Meaning |
|---|---|---|
| `gitlab_ci_job_age_seconds` | project, job_name, status | Age of each currently **running** job (since `started_at`, falling back to `created_at` / run start). GitHub mode adds a `run_id` label. |
| `gitlab_ci_pending_jobs` | project | Count of **pending/queued** jobs per project |
| `gitlab_ci_scrape_errors_total` | project | Poll cycles that failed for that project |
| `gitlab_ci_scrape_duration_seconds` | — | Duration of the last full poll cycle |
| `gitlab_ci_last_scrape_timestamp` | — | Unix time of the last poll attempt (freshness watchdog) |

Plus the standard Go runtime collectors.

## MODE=gitlab

Config via env:

| Var | Default | |
|---|---|---|
| `MODE` | `gitlab` | `gitlab` or `github` |
| `GITLAB_URL` | `https://gitlab.com` | Instance base URL |
| `GITLAB_TOKEN` | — (required) | PAT with `read_api` only |
| `PROJECTS` | `1001:example` | Comma list of `id:shortname` |
| `SCRAPE_INTERVAL` | `60s` | Poll interval |
| `LISTEN_ADDR` | `:8080` | Metrics listen address |

Polling: `GET /api/v4/projects/{id}/jobs?scope[]=running&scope[]=pending&per_page=50`
every `SCRAPE_INTERVAL`. A >50-result page logs a truncation warning. A failed
project poll drops that project's series for the cycle (they go stale) and
bumps `gitlab_ci_scrape_errors_total` — scrapes never block on the API.

## MODE=github

Watches GitHub Actions. Polls
`GET /repos/{org}/{repo}/actions/runs?status=in_progress` and
`?status=queued` (Link-header pagination, `per_page=100`); for every active
run fetches `GET .../runs/{run_id}/jobs` to get the per-job name, status and
`started_at`. Statuses map `in_progress`→running, `queued`→pending.

| Var | Default | |
|---|---|---|
| `MODE` | `gitlab` | `gitlab` or `github` |
| `GITHUB_TOKEN` | — (required) | Fine-grained PAT with read-only Actions access on the watched repos |
| `GITHUB_ORG` | `gyhu-labs` | Org (owner) name |
| `GITHUB_API` | `https://api.github.com` | API base (for Enterprise: `https://HOST/api/v3`) |
| `PROJECTS` | — (required) | Comma list of repo names (no org prefix) |
| `SCRAPE_INTERVAL` | `60s` | Poll interval |
| `LISTEN_ADDR` | `:8080` | Metrics listen address |

Current rate-limit budget: 2 runs-list calls + 1 jobs call per active run per
repo per cycle (pure REST; no GraphQL, no wasteful unfiltered listing).

Endpoints: `/metrics`, `/healthz` (503 until the first successful poll).

## Run

```yaml
# Kubernetes example — Deployment + Service (github mode)
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
          image: ghcr.io/ioseph-ai/ci-exporter:v0.2.0
          ports:
            - name: metrics
              containerPort: 8080
          env:
            - name: MODE
              value: github
            - name: GITHUB_ORG
              value: my-org
            - name: GITHUB_TOKEN
              valueFrom:
                secretKeyRef: { name: ci-exporter, key: token }
            - name: PROJECTS
              value: "service-a,service-b"
```

## Development

```
go build ./... && go vet ./... && go test -race ./...
MODE=github GITHUB_TOKEN=... PROJECTS=repo1 go run .   # then curl :8080/metrics
```
