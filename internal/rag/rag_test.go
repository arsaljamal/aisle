package rag

import (
	"strings"
	"testing"

	"github.com/arsaljamal/aisle/internal/store"
)

func TestCitedKeepsOnlyRetrievedProductsOnce(t *testing.T) {
	hits := []store.Hit{{ID: 7, Name: "oak table"}, {ID: 9, Name: "pine chair"}}
	got := Cited("Try [P9], or [P7]. [P9] is cheaper. [P404] does not exist.", hits)
	if len(got) != 2 || got[0].ID != 9 || got[1].ID != 7 {
		t.Fatalf("got %+v", got)
	}
}

func TestPromptListsEveryProduct(t *testing.T) {
	p := Prompt([]store.Hit{{ID: 1, Name: "sofa"}, {ID: 2, Name: "lamp"}})
	if !strings.Contains(p, "[P1] sofa") || !strings.Contains(p, "[P2] lamp") {
		t.Fatal(p)
	}
}

func TestStripThinking(t *testing.T) {
	if got := stripThinking("<think>hmm\nok</think>\n Answer"); got != "Answer" {
		t.Fatalf("%q", got)
	}
}
