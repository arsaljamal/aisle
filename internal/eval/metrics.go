// Package eval scores ranked results against human relevance labels.
package eval

import (
	"math"
	"sort"
)

// NDCG at k: 1.0 means the best possible ordering of the labelled products.
func NDCG(ranked []int64, gains map[int64]int, k int) float64 {
	dcg := 0.0
	for i, id := range ranked {
		if i >= k {
			break
		}
		dcg += float64(gains[id]) / math.Log2(float64(i+2))
	}
	ideal := make([]int, 0, len(gains))
	for _, g := range gains {
		if g > 0 {
			ideal = append(ideal, g)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ideal)))
	idcg := 0.0
	for i, g := range ideal {
		if i >= k {
			break
		}
		idcg += float64(g) / math.Log2(float64(i+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

// PrecisionExact is the share of the top k that people marked as an exact match.
func PrecisionExact(ranked []int64, gains map[int64]int, k int) float64 {
	if k == 0 {
		return 0
	}
	hit := 0
	for i, id := range ranked {
		if i >= k {
			break
		}
		if gains[id] == 2 {
			hit++
		}
	}
	return float64(hit) / float64(k)
}

// ReciprocalRank is 1/position of the first exact match, 0 if none in the list.
func ReciprocalRank(ranked []int64, gains map[int64]int) float64 {
	for i, id := range ranked {
		if gains[id] == 2 {
			return 1 / float64(i+1)
		}
	}
	return 0
}

// Percentile of already-sorted values, p in [0,1].
func Percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	return sorted[i]
}
