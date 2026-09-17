package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const ghPerPage = 100

// ghWorkflowRun is the subset of the Actions runs-list payload we consume.
// List API fields: run started_at is set for queued runs too (enqueue time);
// run_timing returns run_duration_ms (wall-clock of COMPLETED duration), so
// age is computed from timestamps instead.
type ghWorkflowRun struct {
	ID         int        `json:"id"`
	Name       string     `json:"name"` // workflow name (fallback)
	CreatedAt  *time.Time `json:"created_at"`
	RunStartedAt *time.Time `json:"run_started_at"`
}

// ghJob is one job of a run.
type ghJob struct {
	ID         int        `json:"id"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`     // queued | in_progress | completed
	StartedAt  *time.Time `json:"started_at"` // null while queued
	RunnerName string     `json:"runner_name"`
}

func parseGithubRepos(raw string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("PROJECTS is empty")
	}
	return out, nil
}

type githubClient struct {
	api  string // e.g. https://api.github.com
	org  string
	tok  string
	http *http.Client
}

// get fetches one URL with auth + standard headers. Handles the Link header
// for pagination via nextLink().
func (g *githubClient) get(ctx context.Context, path string, out any) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.api+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return resp, nil
}

// listPage fetches a single page of runs; returns items + the next-page URL
// (empty when exhausted).
func (g *githubClient) listPage(ctx context.Context, path string, out *[]ghWorkflowRun) (string, error) {
	resp, err := g.get(ctx, path, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var payload struct {
		WorkflowRuns []ghWorkflowRun `json:"workflow_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode %s: %w", path, err)
	}
	*out = append(*out, payload.WorkflowRuns...)
	return nextLink(resp.Header.Get("Link")), nil
}

// nextLink extracts rel="next" from an RFC 5988 Link header.
func nextLink(header string) string {
	if header == "" {
		return ""
	}
	for _, part := range strings.Split(header, ",") {
		seg := strings.Split(part, ";")
		if len(seg) != 2 {
			continue
		}
		link := strings.TrimSpace(seg[0])
		params := strings.TrimSpace(seg[1])
		if strings.Contains(params, `rel="next"`) {
			return strings.Trim(link, "<> ")
		}
	}
	return ""
}

// runsForRepo returns all in_progress + queued runs for a repo, following
// pagination.
func (g *githubClient) runsForRepo(ctx context.Context, repo string) ([]ghWorkflowRun, error) {
	var all []ghWorkflowRun
	for _, status := range []string{"in_progress", "queued"} {
		// List workflow runs for a repository. Default per_page=30; ask for
		// the max page size and follow Link.
		path := fmt.Sprintf("/repos/%s/%s/actions/runs?status=%s&per_page=%d", g.org, repo, status, ghPerPage)
		for {
			var runs struct {
				WorkflowRuns []ghWorkflowRun `json:"workflow_runs"`
				TotalCount   int             `json:"total_count"`
			}
			next, err := g.listPage(ctx, path, &runs.WorkflowRuns)
			if err != nil {
				return nil, err
			}
			all = append(all, runs.WorkflowRuns...)
			if next == "" {
				break
			}
			// Link URLs are absolute; convert to a path for the next get().
			u, err := url.Parse(next)
			if err != nil {
				return nil, err
			}
			path = u.Path + "?" + u.RawQuery
		}
	}
	return all, nil
}

// jobsForRun returns up to one page (100) of jobs for a run. Jobs count for
// normal repos is far below the page cap; a full page logs a truncation hint.
func (g *githubClient) jobsForRun(ctx context.Context, repo string, runID int) ([]ghJob, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs?per_page=%d", g.org, repo, runID, ghPerPage)
	resp, err := g.get(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var payload struct {
		Jobs       []ghJob `json:"jobs"`
		TotalCount int     `json:"total_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode jobs for run %d: %w", runID, err)
	}
	if len(payload.Jobs) == ghPerPage {
		log.Printf("WARNING %s run %d: %d jobs (per_page cap) — counts may be truncated", repo, runID, ghPerPage)
	}
	return payload.Jobs, nil
}

// githubPoll runs one full poll cycle across all watched repos and returns
// the resulting snapshot.
func githubPoll(ctx context.Context, cli *githubClient, repos []string) *snapshot {
	var (
		running []jobSnapshot
		pending = make(map[string]float64)
	)
	now := time.Now()
	for _, repo := range repos {
		runs, err := cli.runsForRepo(ctx, repo)
		if err != nil {
			scrapeErrors.WithLabelValues(repo).Inc()
			log.Printf("ERROR repo %s: %v", repo, err)
			continue
		}
		var pend float64
		for _, r := range runs {
			jobs, err := cli.jobsForRun(ctx, repo, r.ID)
			if err != nil {
				scrapeErrors.WithLabelValues(repo).Inc()
				log.Printf("ERROR repo %s run %d: %v", repo, r.ID, err)
				continue
			}
			for _, j := range jobs {
				switch j.Status {
				case "queued":
					pend++
				case "in_progress":
					base := j.StartedAt
					if base == nil {
						base = r.RunStartedAt
					}
					if base == nil {
						base = r.CreatedAt
					}
					if base == nil {
						continue
					}
					running = append(running, jobSnapshot{
						project: repo,
						name:    j.Name,
						age:     now.Sub(*base),
						runID:   strconv.Itoa(r.ID),
					})
				}
			}
		}
		pending[repo] = pend
	}
	return &snapshot{running: running, pending: pending, project: repos}
}

func runGithub() {
	token := os.Getenv("GITHUB_TOKEN")
	org := envOr("GITHUB_ORG", "gyhu-labs")
	api := envOr("GITHUB_API", "https://api.github.com")
	rawProjects := envOr("PROJECTS", "")
	if token == "" {
		log.Fatal("GITHUB_TOKEN must be set")
	}
	if rawProjects == "" {
		log.Fatal("PROJECTS must be set (comma-separated repo names) in github mode")
	}
	repos, err := parseGithubRepos(rawProjects)
	if err != nil {
		log.Fatalf("bad PROJECTS %q: %v", rawProjects, err)
	}

	coll := newCollector(true)
	cli := &githubClient{
		api:  strings.TrimRight(api, "/"),
		org:  org,
		tok:  token,
		http: &http.Client{Timeout: 15 * time.Second},
	}

	poll := func(ctx context.Context) {
		start := time.Now()
		snap := githubPoll(ctx, cli, repos)
		coll.publish(snap, time.Since(start))
	}

	log.Printf("github mode: org %s, api %s, %d repos", org, api, len(repos))
	serve(coll, poll, scrapeInterval(), listenAddr(), "ci-exporter(github)")
}
