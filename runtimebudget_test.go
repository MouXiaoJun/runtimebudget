package runtimebudget

import (
	"errors"
	"math"
	"runtime/metrics"
	"testing"
	"time"
)

const (
	uintMetric   = "/gc/cycles/total:gc-cycles"
	floatMetric  = "/cpu/classes/total:cpu-seconds"
	budgetMetric = "/gc/heap/goal:bytes"
)

func TestReadAndKindSafeValue(t *testing.T) {
	snapshot := Read(uintMetric, "runtimebudget/unknown")

	value, ok := snapshot.Get(uintMetric)
	if !ok || value.Kind() != metrics.KindUint64 {
		t.Fatalf("Get(%q) = %#v, %v", uintMetric, value, ok)
	}
	if _, ok := value.Float64(); ok {
		t.Fatal("Float64 succeeded for a uint64 value")
	}
	if _, ok := value.Histogram(); ok {
		t.Fatal("Histogram succeeded for a uint64 value")
	}

	unknown, ok := snapshot.Get("runtimebudget/unknown")
	if !ok || unknown.Kind() != metrics.KindBad {
		t.Fatalf("unknown metric = %#v, %v; want present KindBad", unknown, ok)
	}
	if _, ok := unknown.Uint64(); ok {
		t.Fatal("Uint64 succeeded for KindBad")
	}
}

func TestReadAllAndNames(t *testing.T) {
	snapshot := ReadAll()
	if len(snapshot.Names()) == 0 {
		t.Fatal("ReadAll returned no metrics")
	}
	if _, ok := snapshot.Get(uintMetric); !ok {
		t.Fatalf("ReadAll does not contain %q", uintMetric)
	}
}

func TestDeltaAndRate(t *testing.T) {
	previousAt := time.Unix(10, 0)
	currentAt := previousAt.Add(2 * time.Second)
	previous := testSnapshot(previousAt, map[string]Value{
		uintMetric:  {kind: metrics.KindUint64, uint64: 10},
		floatMetric: {kind: metrics.KindFloat64, float64: 1.5},
	})
	current := testSnapshot(currentAt, map[string]Value{
		uintMetric:  {kind: metrics.KindUint64, uint64: 17},
		floatMetric: {kind: metrics.KindFloat64, float64: 4.5},
	})

	delta, err := Delta(previous, current, uintMetric)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := delta.Uint64(); !ok || got != 7 {
		t.Fatalf("uint delta = %d, %v; want 7, true", got, ok)
	}

	rate, err := Rate(previous, current, floatMetric)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 1.5 {
		t.Fatalf("float rate = %g, want 1.5", rate)
	}
}

func TestDeltaErrorsAreSafe(t *testing.T) {
	snapshot := testSnapshot(time.Unix(1, 0), map[string]Value{
		uintMetric:   {kind: metrics.KindUint64, uint64: 2},
		budgetMetric: {kind: metrics.KindUint64, uint64: 10},
	})
	current := testSnapshot(time.Unix(2, 0), map[string]Value{
		budgetMetric: {kind: metrics.KindUint64, uint64: 11},
	})

	if _, err := Delta(snapshot, current, budgetMetric); !errors.Is(err, ErrNotCumulative) {
		t.Fatalf("non-cumulative Delta error = %v", err)
	}
	if _, err := Delta(snapshot, current, "runtimebudget/unknown"); !errors.Is(err, ErrUnknownMetric) {
		t.Fatalf("unknown Delta error = %v", err)
	}
	if _, err := Delta(snapshot, current, uintMetric); !errors.Is(err, ErrMetricMissing) {
		t.Fatalf("missing Delta error = %v", err)
	}

	reset := testSnapshot(time.Unix(2, 0), map[string]Value{
		uintMetric: {kind: metrics.KindUint64, uint64: 1},
	})
	complete := testSnapshot(time.Unix(3, 0), map[string]Value{
		uintMetric: {kind: metrics.KindUint64, uint64: 0},
	})
	if _, err := Delta(reset, complete, uintMetric); !errors.Is(err, ErrCounterReset) {
		t.Fatalf("reset Delta error = %v", err)
	}
}

func TestHistogramDelta(t *testing.T) {
	previous := testSnapshot(time.Unix(1, 0), map[string]Value{
		"/sched/latencies:seconds": histogramValue([]uint64{2, 3}, []float64{0, 1, math.Inf(1)}),
	})
	current := testSnapshot(time.Unix(2, 0), map[string]Value{
		"/sched/latencies:seconds": histogramValue([]uint64{5, 8}, []float64{0, 1, math.Inf(1)}),
	})

	name := "/sched/latencies:seconds"
	delta, err := HistogramDelta(previous, current, name)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Counts) != 2 || delta.Counts[0] != 3 || delta.Counts[1] != 5 {
		t.Fatalf("histogram delta = %#v", delta)
	}

	if _, err := Delta(previous, current, name); !errors.Is(err, ErrHistogramScalarDelta) {
		t.Fatalf("histogram scalar Delta error = %v", err)
	}
}

func TestCheck(t *testing.T) {
	snapshot := testSnapshot(time.Unix(1, 0), map[string]Value{
		budgetMetric: {kind: metrics.KindUint64, uint64: 12},
		uintMetric:   {kind: metrics.KindUint64, uint64: 1},
	})
	report := Check(snapshot, map[string]float64{
		budgetMetric: 10,
		uintMetric:   100,
	})
	if report.OK() || len(report.Violations) != 1 || report.Violations[0].Name != budgetMetric {
		t.Fatalf("report = %#v", report)
	}
	if !errors.Is(report.Err(), ErrBudgetExceeded) {
		t.Fatalf("report error = %v", report.Err())
	}
	if !errors.Is(Check(snapshot, map[string]float64{"runtimebudget/unknown": 1}).Err(), ErrUnknownMetric) {
		t.Fatal("unknown budget was not reported")
	}
	if !errors.Is(Check(snapshot, map[string]float64{budgetMetric: math.NaN()}).Err(), ErrInvalidBudget) {
		t.Fatal("invalid budget was not reported")
	}
}

func TestReportOK(t *testing.T) {
	if err := Check(testSnapshot(time.Unix(1, 0), map[string]Value{
		budgetMetric: {kind: metrics.KindUint64, uint64: 10},
	}), map[string]float64{budgetMetric: 10}).Err(); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkReadOne(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = Read(uintMetric)
	}
}

func testSnapshot(at time.Time, values map[string]Value) Snapshot {
	return Snapshot{at: at, values: values}
}

func histogramValue(counts []uint64, buckets []float64) Value {
	return Value{kind: metrics.KindFloat64Histogram, histogram: Histogram{Counts: counts, Buckets: buckets}}
}
