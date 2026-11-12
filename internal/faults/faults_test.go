package faults

import "testing"

func TestGrade(t *testing.T) {
	report := "## Symptom\nslow\n## Root cause\nQuery embedding got slow after the embedding host maintenance window.\n## Evidence\nconnection pool was fine"
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

func TestGradeLLMDownNeedsTheModel(t *testing.T) {
	wrong := "## Root cause\nThe rollout of 1.4.0 broke fusion, so the Ask endpoint fails.\n## Evidence\n"
	if g := Grade(wrong, []string{LLMDown}); g != "wrong" {
		t.Fatalf("got %s", g)
	}
	right := "## Root cause\nconfig reloaded: llm.endpoint_changed=true; the answer model is unreachable\n## Evidence\n"
	if g := Grade(right, []string{LLMDown}); g != "correct" {
		t.Fatalf("got %s", g)
	}
}

func TestGradeListingEveryChangeIsMixed(t *testing.T) {
	r := "## Root cause\nThe embedding host maintenance window, plus a config reload that set db.max_conns=1.\n## Evidence\n"
	if g := Grade(r, []string{SlowEmbed}); g != "mixed" {
		t.Fatalf("got %s", g)
	}
}
