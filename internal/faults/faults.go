// Package faults holds the break switches: realistic failures that can be
// turned on at runtime so an on-call engineer (or Drill Down Harness) has
// something real to investigate.
package faults

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
)

const (
	SlowEmbed     = "slow-embed"     // query embedding takes ~1.2 s longer
	DBPool        = "db-pool"        // database access squeezed through one slot
	ModelMismatch = "model-mismatch" // query vectors come from a different model than the index
	BadDeploy     = "bad-deploy"     // a new version fails a share of hybrid searches
	LLMDown       = "llm-down"       // the answer model is unreachable
)

// Catalogue describes each switch in plain words.
var Catalogue = map[string]string{
	SlowEmbed:     "query embedding takes about 1.2 s longer",
	DBPool:        "all database work goes through a single slot",
	ModelMismatch: "queries are embedded with a different model than the index (quality drops, no errors)",
	BadDeploy:     "version 1.4.1 fails about 20% of hybrid searches",
	LLMDown:       "the answer model is unreachable, so Ask fails",
}

type Set struct {
	mu sync.RWMutex
	on map[string]bool
}

func New() *Set { return &Set{on: map[string]bool{}} }

func (s *Set) On(name string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.on[name]
}

func (s *Set) Set(name string, on bool) error {
	if _, ok := Catalogue[name]; !ok {
		return fmt.Errorf("unknown fault %q", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.on[name] = on
	return nil
}

// Active lists the switches that are on, sorted.
func (s *Set) Active() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for k, v := range s.on {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// RootCause says what a correct investigation must name for each switch.
// Used to grade reports automatically; matched against the root-cause section.
var RootCause = map[string]*regexp.Regexp{
	SlowEmbed:     regexp.MustCompile(`(?i)embedding (host|service|latency|time|call)|maintenance|slow embed`),
	DBPool:        regexp.MustCompile(`(?i)max_conns|connection pool|db pool|database (connection|contention|pool)|pool (size|exhaust|starv)`),
	ModelMismatch: regexp.MustCompile(`(?i)v2-preview|embedding model|query_model|model (change|mismatch)`),
	BadDeploy:     regexp.MustCompile(`(?i)(version|rollout of|deploy(ment)? of|release) (v)?1\.4\.1|1\.4\.1 (rollout|deploy|release)`),
	LLMDown:       regexp.MustCompile(`(?i)\bllm\b|llm\.endpoint|endpoint_changed|answer model|chat model|chat/completions|connection refused`),
}

// Grade checks the root-cause section of a report against the active switches.
// It returns "" when no switch is on (nothing to grade).
func Grade(report string, active []string) (verdict string) {
	if len(active) == 0 {
		return ""
	}
	section := report
	low := strings.ToLower(report)
	if i := strings.Index(low, "## root cause"); i >= 0 {
		section = report[i:]
		if j := strings.Index(strings.ToLower(section[3:]), "## "); j >= 0 {
			section = section[:j+3]
		}
	} else {
		return "no root cause section"
	}
	// Correct means the true cause is named and no other switch's cause is:
	// a report that lists every recent change has not found the root cause.
	named := map[string]bool{}
	for f, re := range RootCause {
		if re.MatchString(section) {
			named[f] = true
		}
	}
	for _, f := range active {
		if named[f] {
			for other := range named {
				if other != f && !slices.Contains(active, other) {
					return "mixed"
				}
			}
			return "correct"
		}
	}
	return "wrong"
}
