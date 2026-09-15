// ci-exporter — Prometheus exporter for GitLab CI job states.
//
// Config via env: GITLAB_URL, GITLAB_TOKEN, PROJECTS="1001:alpha,1002:beta",
// SCRAPE_INTERVAL, LISTEN_ADDR.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
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

	snap atomic.Pointer[snapshot]
}

// gitlabJob is the subset of the jobs-API payload we consume.
type gitlabJob struct {
	ID        int        `json:"id"`
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	StartedAt *time.Time `json:"started_at"`
	CreatedAt *time.Time `json:"created_at"`
	WebURL    string     `json:"web_url"`
}

func newCollector() *collector {
	return &collector{
		runningInfo: prometheus.NewDesc(
			"gitlab_ci_job_age_seconds",
			"Age in seconds of a currently running GitLab CI job (since started_at).",
			[]string{"project", "job_name", "status"}, nil,
		),
		pendingInfo: prometheus.NewDesc(
			"gitlab_ci_pending_jobs",
			"Number of GitLab CI jobs currently pending per project (>0 for a while usually means a dead runner).",
			[]string{"project"}, nil,
		),
	}
}

// poll runs one cycle: fetch running+pending jobs for all projects, publish a
// fresh snapshot, update cycle gauges/counters. Errors are per-project: a
// failing project is dropped from that cycle's snapshot (its series go stale)
// and counted in gitlab_ci_scrape_errors_total.
func (c *collector) poll(ctx context.Context, cli *gitlabClient, projects []projectRef) {
	start := time.Now()
	var (
		running []jobSnapshot
		pending = make(map[string]float64)
	)
	for _, p := range projects {
		jobs, err := cli.jobs(ctx, p.id)
		if err != nil {
			scrapeErrors.WithLabelValues(p.name).Inc()
			log.Printf("ERROR project %s (%d): %v", p.name, p.id, err)
			continue
		}
		now := time.Now()
		var pend float64
		for _, j := range jobs {
			switch j.Status {
			case "pending":
				pend++
			case "running":
				base := j.StartedAt
				if base == nil {
					base = j.CreatedAt
				}
				if base == nil {
					continue
				}
				running = append(running, jobSnapshot{
					project: p.name,
					name:    j.Name,
					age:     now.Sub(*base),
				})
			}
		}
		pending[p.name] = pend
	}

	names := make([]string, 0, len(projects))
	for _, p := range projects {
		names = append(names, p.name)
	}
	c.snap.Store(&snapshot{running: running, pending: pending, project: names})

	scrapeDuration.Set(time.Since(start).Seconds())
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
		ch <- prometheus.MustNewConstMetric(c.runningInfo, prometheus.GaugeValue, j.age.Seconds(), j.project, j.name, "running")
	}
	for _, p := range s.project {
		ch <- prometheus.MustNewConstMetric(c.pendingInfo, prometheus.GaugeValue, s.pending[p], p)
	}
}

type projectRef struct {
	id   int
	name string
}

func parseProjects(raw string) ([]projectRef, error) {
	var out []projectRef
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idName := strings.SplitN(part, ":", 2)
		if len(idName) != 2 {
			return nil, fmt.Errorf("PROJECTS entry %q is not id:name", part)
		}
		id, err := strconv.Atoi(strings.TrimSpace(idName[0]))
		if err != nil {
			return nil, fmt.Errorf("PROJECTS entry %q has non-numeric id: %v", part, err)
		}
		name := strings.TrimSpace(idName[1])
		if name == "" {
			return nil, fmt.Errorf("PROJECTS entry %q has empty name", part)
		}
		out = append(out, projectRef{id: id, name: name})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("PROJECTS is empty")
	}
	return out, nil
}

type gitlabClient struct {
	base string
	tok  string
	http *http.Client
}

func (g *gitlabClient) jobs(ctx context.Context, projectID int) ([]gitlabJob, error) {
	url := fmt.Sprintf("%s/api/v4/projects/%d/jobs?scope%%5B%%5D=running&scope%%5B%%5D=pending&per_page=50", g.base, projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", g.tok)
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jobs API: HTTP %d", resp.StatusCode)
	}
	var jobs []gitlabJob
	if err := json.NewDecoder(resp.Body).Decode(&jobs); err != nil {
		return nil, fmt.Errorf("decode jobs: %w", err)
	}
	if len(jobs) == 50 {
		log.Printf("WARNING project %d: 50 running/pending jobs (per_page cap) — counts may be truncated", projectID)
	}
	return jobs, nil
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

func main() {
	listen := envOr("LISTEN_ADDR", ":8080")
	base := envOr("GITLAB_URL", "https://gitlab.com")
	token := os.Getenv("GITLAB_TOKEN")
	interval := envOr("SCRAPE_INTERVAL", "60s")
	rawProjects := envOr("PROJECTS", "1001:example")
	if token == "" {
		log.Fatal("GITLAB_TOKEN must be set")
	}
	projects, err := parseProjects(rawProjects)
	if err != nil {
		log.Fatalf("bad PROJECTS %q: %v", rawProjects, err)
	}
	dur, err := time.ParseDuration(interval)
	if err != nil {
		log.Fatalf("bad SCRAPE_INTERVAL %q: %v", interval, err)
	}

	coll := newCollector()
	prometheus.MustRegister(coll)

	cli := &gitlabClient{base: strings.TrimRight(base, "/"), tok: token, http: &http.Client{Timeout: 15 * time.Second}}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if coll.snap.Load() == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintln(w, "no successful poll yet")
			return
		}
		_, _ = fmt.Fprintln(w, "ok")
	})

	ctx := context.Background()
	go func() {
		// Poll immediately, then on the interval; a failed cycle keeps serving
		// the previous snapshot (freshness visible via gitlab_ci_last_scrape_timestamp).
		for {
			coll.poll(ctx, cli, projects)
			select {
			case <-time.After(dur):
			case <-ctx.Done():
				return
			}
		}
	}()

	log.Printf("ci-exporter: listening on %s, %d projects, interval %s, url %s",
		listen, len(projects), dur, base)
	log.Fatal(http.ListenAndServe(listen, mux))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
