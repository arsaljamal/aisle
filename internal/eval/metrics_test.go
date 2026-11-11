package eval

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestNDCGPerfectOrderIsOne(t *testing.T) {
	gains := map[int64]int{1: 2, 2: 1, 3: 0}
	if got := NDCG([]int64{1, 2, 3}, gains, 10); !near(got, 1) {
		t.Fatalf("got %v", got)
	}
}

func TestNDCGPenalisesBadOrder(t *testing.T) {
	gains := map[int64]int{1: 2, 2: 1}
	best := NDCG([]int64{1, 2}, gains, 10)
	worse := NDCG([]int64{2, 1}, gains, 10)
	if !(worse < best) {
		t.Fatalf("best %v worse %v", best, worse)
	}
}

func TestNDCGUnlabelledIsZeroGain(t *testing.T) {
	if got := NDCG([]int64{9, 8}, map[int64]int{1: 2}, 10); got != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestPrecisionAndRR(t *testing.T) {
	gains := map[int64]int{1: 1, 2: 2}
	if got := PrecisionExact([]int64{1, 2}, gains, 2); !near(got, 0.5) {
		t.Fatalf("precision %v", got)
	}
	if got := ReciprocalRank([]int64{1, 2}, gains); !near(got, 0.5) {
		t.Fatalf("rr %v", got)
	}
}

func TestPercentile(t *testing.T) {
	v := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if Percentile(v, 0.5) != 5 || Percentile(v, 0.95) != 10 {
		t.Fatal("percentile")
	}
}
