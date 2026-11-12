package faults

import "testing"

func TestGrade(t *testing.T) {
	report := "## Symptom\nslow\n## Root cause\nQuery embedding got slow after a maintenance window.\n## Evidence\ndatabase was fine"
	if g := Grade(report, []string{SlowEmbed}); g != "correct" {
		t.Fatalf("got %s", g)
	}
	// "database" appears only in Evidence, so naming the pool is not credited.
	if g := Grade(report, []string{DBPool}); g != "wrong" {
		t.Fatalf("got %s", g)
	}
	if g := Grade("no headings", []string{DBPool}); g != "no root cause section" {
		t.Fatalf("got %s", g)
	}
	if g := Grade(report, nil); g != "" {
		t.Fatalf("got %s", g)
	}
}
