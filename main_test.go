package main

import (
	"strings"
	"testing"
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
