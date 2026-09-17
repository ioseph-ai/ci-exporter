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
	"time"
)

// jobRef is one watched project/repo.
type jobRef struct {
	id   int
	name string
}

func parseProjects(raw string) ([]jobRef, error) {
	var out []jobRef
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
		out = append(out, jobRef{id: id, name: name})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("PROJECTS is empty")
	}
	return out, nil
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

func runGitlab() {
	base := envOr("GITLAB_URL", "https://gitlab.gyhu.me")
	token := os.Getenv("GITLAB_TOKEN")
	rawProjects := envOr("PROJECTS", "6:cp,7:dev,9:gw,13:intel,14:portal")
	if token == "" {
		log.Fatal("GITLAB_TOKEN must be set")
	}
	projects, err := parseProjects(rawProjects)
	if err != nil {
		log.Fatalf("bad PROJECTS %q: %v", rawProjects, err)
	}

	coll := newCollector(false)
	cli := &gitlabClient{base: strings.TrimRight(base, "/"), tok: token, http: &http.Client{Timeout: 15 * time.Second}}

	poll := func(ctx context.Context) {
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
		coll.publish(&snapshot{running: running, pending: pending, project: names}, time.Since(start))
	}

	log.Printf("gitlab mode: url %s, %d projects", base, len(projects))
	serve(coll, poll, scrapeInterval(), listenAddr(), "gitlab-ci-exporter")
}
