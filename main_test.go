package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestParseProjects(t *testing.T) {
	got, err := parseProjects("1001:alpha, 1002:beta,1003:gamma")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantIDs := []int{1001, 1002, 1003}
	wantNames := []string{"alpha", "beta", "gamma"}
	for i, p := range got {
		if p.id != wantIDs[i] || p.name != wantNames[i] {
			t.Errorf("entry %d: got {%d %s}, want {%d %s}", i, p.id, p.name, wantIDs[i], wantNames[i])
		}
	}
}

func TestParseProjectsErrors(t *testing.T) {
	cases := map[string]string{
		"":           "PROJECTS is empty",
		"1001":       "not id:name",
		"1001:":      "empty name",
		"thousand:a": "non-numeric id",
	}
	for raw, wantSub := range cases {
		if _, err := parseProjects(raw); err == nil {
			t.Errorf("parseProjects(%q): expected error", raw)
		} else if !strings.Contains(err.Error(), wantSub) {
			t.Errorf("parseProjects(%q): error %q does not contain %q", raw, err.Error(), wantSub)
		}
	}
}

func TestParseGithubRepos(t *testing.T) {
	got, err := parseGithubRepos("alpha, beta,,alpha")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("got %v, want [alpha beta] (dedup + blanks stripped)", got)
	}
	if _, err := parseGithubRepos(""); err == nil {
		t.Error("expected error for empty PROJECTS")
	}
}

func TestNextLink(t *testing.T) {
	cases := map[string]string{
		"": "",
		`<https://api.github.com/repositories/1/actions/runs?page=2>; rel="next", <https://api.github.com/repositories/1/actions/runs?page=5>; rel="last"`: "https://api.github.com/repositories/1/actions/runs?page=2",
		`<https://api.github.com/x>; rel="prev"`: "",
	}
	for header, want := range cases {
		if got := nextLink(header); got != want {
			t.Errorf("nextLink(%q) = %q, want %q", header, got, want)
		}
	}
}

func newFakeGH(t *testing.T) *httptest.Server {
	t.Helper()
	timePtr := func(ts time.Time) *time.Time { return &ts }
	m := http.NewServeMux()
	m.HandleFunc("/repos/org/repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		status := r.URL.Query().Get("status")
		var runs []ghWorkflowRun
		w.Header().Set("Link", `<https://127.0.0.1/never>; rel="last"`)
		switch status {
		case "in_progress":
			if page != "2" {
				// Page 1 advertises a next page (Link-following exercise).
				w.Header().Set("Link", `<`+"http://"+r.Host+r.URL.Path+`?status=in_progress&per_page=100&page=2>; rel="next"`)
				runs = []ghWorkflowRun{{ID: 11, RunStartedAt: timePtr(time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC))}}
			}
		case "queued":
			runs = []ghWorkflowRun{{ID: 12, RunStartedAt: timePtr(time.Date(2026, 9, 17, 9, 5, 0, 0, time.UTC))}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "workflow_runs": runs})
	})
	m.HandleFunc("/repos/org/repo/actions/runs/11/jobs", func(w http.ResponseWriter, _ *http.Request) {
		started := time.Date(2026, 9, 17, 9, 1, 0, 0, time.UTC)
		jobs := []ghJob{
			{ID: 111, Name: "build", Status: "in_progress", StartedAt: &started},
			{ID: 112, Name: "lint", Status: "queued"},
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "jobs": jobs})
	})
	m.HandleFunc("/repos/org/repo/actions/runs/12/jobs", func(w http.ResponseWriter, _ *http.Request) {
		jobs := []ghJob{
			// in_progress with no per-job started_at → falls back to the
			// run-level run_started_at.
			{ID: 121, Name: "e2e", Status: "in_progress"},
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "jobs": jobs})
	})
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return srv
}

func TestGithubPoll(t *testing.T) {
	srv := newFakeGH(t)
	cli := &githubClient{api: srv.URL, org: "org", tok: "t", http: srv.Client()}

	// Sanity: pagination is actually followed.
	runs, err := cli.runsForRepo(t.Context(), "repo")
	if err != nil {
		t.Fatalf("runsForRepo: %v", err)
	}
	if len(runs) == 0 {
		t.Fatal("no runs returned")
	}

	snap := githubPoll(t.Context(), cli, []string{"repo"})
	if len(snap.running) != 2 {
		t.Fatalf("running jobs = %d, want 2", len(snap.running))
	}
	if snap.pending["repo"] != 1 {
		t.Errorf("pending = %v, want 1", snap.pending["repo"])
	}
	names := map[string]bool{}
	for _, j := range snap.running {
		names[j.name] = true
		if j.runID == "" {
			t.Errorf("in-progress job %q missing run_id", j.name)
		}
		if j.age < 0 {
			t.Errorf("job %q negative age %v", j.name, j.age)
		}
	}
	if !names["build"] || !names["e2e"] {
		t.Errorf("jobs = %v, want build + e2e (run_started_at fallback for e2e)", names)
	}
}

func TestCollectGithubMode(t *testing.T) {
	coll := newCollector(true)
	coll.snap.Store(&snapshot{
		running: []jobSnapshot{{project: "repo", name: "build", age: 30 * time.Second, runID: "11"}},
		pending: map[string]float64{"repo": 1},
		project: []string{"repo"},
	})
	if n := testutil.CollectAndCount(coll); n == 0 {
		t.Fatal("collect produced no metrics")
	}
	if err := testutil.CollectAndCompare(coll, strings.NewReader(`
# HELP gitlab_ci_job_age_seconds Age in seconds of a currently running CI job (since started_at, falling back to the run/pipeline creation time).
# TYPE gitlab_ci_job_age_seconds gauge
gitlab_ci_job_age_seconds{job_name="build",project="repo",run_id="11",status="running"} 30
# HELP gitlab_ci_pending_jobs Number of CI jobs currently pending per project (>0 for a while usually means a dead runner).
# TYPE gitlab_ci_pending_jobs gauge
gitlab_ci_pending_jobs{project="repo"} 1
`), "gitlab_ci_job_age_seconds", "gitlab_ci_pending_jobs"); err != nil {
		t.Errorf("github-mode metric output mismatch: %v", err)
	}
}

func TestCollectGitlabMode(t *testing.T) {
	coll := newCollector(false)
	coll.snap.Store(&snapshot{
		running: []jobSnapshot{{project: "cp", name: "build", age: 30 * time.Second}},
		pending: map[string]float64{"cp": 0},
		project: []string{"cp"},
	})
	if n := testutil.CollectAndCount(coll); n == 0 {
		t.Fatal("collect produced no metrics")
	}
	if err := testutil.CollectAndCompare(coll, strings.NewReader(`
# HELP gitlab_ci_job_age_seconds Age in seconds of a currently running CI job (since started_at, falling back to the run/pipeline creation time).
# TYPE gitlab_ci_job_age_seconds gauge
gitlab_ci_job_age_seconds{job_name="build",project="cp",status="running"} 30
# HELP gitlab_ci_pending_jobs Number of CI jobs currently pending per project (>0 for a while usually means a dead runner).
# TYPE gitlab_ci_pending_jobs gauge
gitlab_ci_pending_jobs{project="cp"} 0
`), "gitlab_ci_job_age_seconds", "gitlab_ci_pending_jobs"); err != nil {
		t.Errorf("gitlab-mode metric output mismatch: %v", err)
	}
}
