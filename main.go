// ci-exporter — Prometheus exporter for CI job states (GitLab CI or GitHub
// Actions), selected with MODE.
//
// MODE=gitlab (default): polls the GitLab jobs API for running/pending jobs
// of a fixed project list. Config via env: GITLAB_URL, GITLAB_TOKEN,
// PROJECTS="1001:alpha,1002:beta", SCRAPE_INTERVAL, LISTEN_ADDR.
//
// MODE=github: polls the GitHub Actions REST API
// (/repos/{org}/{repo}/actions/runs + /runs/{run_id}/jobs) for in_progress
// and queued jobs. Metric names and labels are IDENTICAL to the GitLab mode
// (project = repo name) so existing alert rules fire unchanged, with an
// extra run_id label. Config via env: GITHUB_TOKEN, GITHUB_ORG,
// GITHUB_API, PROJECTS="repo1,repo2", SCRAPE_INTERVAL, LISTEN_ADDR.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// jobSnapshot is one running job from the last successful poll cycle.
type jobSnapshot struct {
	project string
	name    string
	age     time.Duration
	// runID is only populated in github mode (extra label; the alert rules
	// never select on it).
	runID string
}

// snapshot is the immutable result of one poll cycle, published atomically.
// Series absent from the current snapshot simply go stale on the next scrape —
// no per-series reset races.
type snapshot struct {
	running []jobSnapshot
	pending map[string]float64
	project []string
}

type collector struct {
	runningInfo *prometheus.Desc
	pendingInfo *prometheus.Desc

	withRunID bool
	snap      atomic.Pointer[snapshot]
}

// newCollector builds the metric descriptors. Metric names are mode-stable
// (gitlab_ci_*) by design: all alert rules (GitLabJobRunningTooLong and
// friends) keep firing unchanged when the exporter is flipped to github mode.
// withRunID adds the run_id label to job_age_seconds (github mode only).
func newCollector(withRunID bool) *collector {
	runningLabels := []string{"project", "job_name", "status"}
	if withRunID {
		runningLabels = append(runningLabels, "run_id")
	}
	return &collector{
		withRunID:  withRunID,
		runningInfo: prometheus.NewDesc(
			"gitlab_ci_job_age_seconds",
			"Age in seconds of a currently running CI job (since started_at, falling back to the run/pipeline creation time).",
			runningLabels, nil,
		),
		pendingInfo: prometheus.NewDesc(
			"gitlab_ci_pending_jobs",
			"Number of CI jobs currently pending per project (>0 for a while usually means a dead runner).",
			[]string{"project"}, nil,
		),
	}
}

// publish stores a fresh snapshot and updates the cycle gauges.
func (c *collector) publish(s *snapshot, cycleDur time.Duration) {
	c.snap.Store(s)
	scrapeDuration.Set(cycleDur.Seconds())
	lastScrapeTime.SetToCurrentTime()
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.runningInfo
	ch <- c.pendingInfo
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	s := c.snap.Load()
	if s == nil {
		return
	}
	for _, j := range s.running {
		labels := []string{j.project, j.name, "running"}
		if c.withRunID {
			labels = append(labels, j.runID)
		}
		ch <- prometheus.MustNewConstMetric(c.runningInfo, prometheus.GaugeValue, j.age.Seconds(), labels...)
	}
	for _, p := range s.project {
		ch <- prometheus.MustNewConstMetric(c.pendingInfo, prometheus.GaugeValue, s.pending[p], p)
	}
}

var (
	scrapeErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gitlab_ci_scrape_errors_total",
		Help: "Failed poll cycles per project (project dropped from snapshot for that cycle).",
	}, []string{"project"})
	scrapeDuration = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gitlab_ci_scrape_duration_seconds",
		Help: "Duration of the last full poll cycle.",
	})
	lastScrapeTime = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gitlab_ci_last_scrape_timestamp",
		Help: "Unix timestamp of the last poll cycle attempt.",
	})
)

// serve wires the collector, the poll loop and the HTTP endpoints. A failed
// cycle keeps serving the previous snapshot (freshness visible via
// gitlab_ci_last_scrape_timestamp).
func serve(coll *collector, poll func(ctx context.Context), interval time.Duration, listen string, logPrefix string) {
	prometheus.MustRegister(coll)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if coll.snap.Load() == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("no successful poll yet\n"))
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})

	ctx := context.Background()
	go func() {
		for {
			poll(ctx)
			select {
			case <-time.After(interval):
			case <-ctx.Done():
				return
			}
		}
	}()

	log.Printf("%s: listening on %s, interval %s", logPrefix, listen, interval)
	log.Fatal(http.ListenAndServe(listen, mux))
}

func scrapeInterval() time.Duration {
	v := envOr("SCRAPE_INTERVAL", "60s")
	dur, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("bad SCRAPE_INTERVAL %q: %v", v, err)
	}
	return dur
}

func listenAddr() string { return envOr("LISTEN_ADDR", ":8080") }

func main() {
	mode := strings.ToLower(envOr("MODE", "gitlab"))
	switch mode {
	case "gitlab":
		runGitlab()
	case "github":
		runGithub()
	default:
		log.Fatalf("bad MODE %q (want gitlab|github)", mode)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
