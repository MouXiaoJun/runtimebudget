package runtimebudget

import (
	"context"
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

func TestCheckDelta(t *testing.T) {
	previous := testSnapshot(time.Unix(10, 0), map[string]Value{
		uintMetric:                 {kind: metrics.KindUint64, uint64: 10},
		floatMetric:                {kind: metrics.KindFloat64, float64: 1.5},
		budgetMetric:               {kind: metrics.KindUint64, uint64: 10},
		"/sched/latencies:seconds": histogramValue([]uint64{2, 3}, []float64{0, 1, math.Inf(1)}),
	})
	current := testSnapshot(time.Unix(12, 0), map[string]Value{
		uintMetric:                 {kind: metrics.KindUint64, uint64: 17},
		floatMetric:                {kind: metrics.KindFloat64, float64: 4.5},
		budgetMetric:               {kind: metrics.KindUint64, uint64: 11},
		"/sched/latencies:seconds": histogramValue([]uint64{5, 8}, []float64{0, 1, math.Inf(1)}),
	})

	report := CheckDelta(previous, current, map[string]float64{
		uintMetric:                 6,
		floatMetric:                2,
		budgetMetric:               1,
		"/sched/latencies:seconds": 1,
		"runtimebudget/unknown":    1,
	})
	if len(report.Violations) != 2 || report.Violations[0].Name != floatMetric || report.Violations[1].Name != uintMetric {
		t.Fatalf("delta violations = %#v", report.Violations)
	}
	if len(report.Issues) != 3 || report.Issues[0].Name != budgetMetric || report.Issues[1].Name != "/sched/latencies:seconds" || report.Issues[2].Name != "runtimebudget/unknown" {
		t.Fatalf("delta issues = %#v", report.Issues)
	}
	if !errors.Is(report.Err(), ErrNotCumulative) || !errors.Is(report.Err(), ErrHistogramScalarDelta) || !errors.Is(report.Err(), ErrUnknownMetric) {
		t.Fatalf("delta report error = %v", report.Err())
	}

	reset := testSnapshot(time.Unix(12, 0), map[string]Value{uintMetric: {kind: metrics.KindUint64, uint64: 1}})
	if !errors.Is(CheckDelta(previous, reset, map[string]float64{uintMetric: 1}).Err(), ErrCounterReset) {
		t.Fatal("counter reset was not reported by CheckDelta")
	}

	invalid := CheckDelta(previous, current, map[string]float64{uintMetric: math.NaN(), floatMetric: math.Inf(1)})
	if len(invalid.Issues) != 2 || !errors.Is(invalid.Err(), ErrInvalidBudget) {
		t.Fatalf("invalid delta budgets = %#v", invalid)
	}
}

func TestCheckRate(t *testing.T) {
	previous := testSnapshot(time.Unix(10, 0), map[string]Value{
		uintMetric:  {kind: metrics.KindUint64, uint64: 10},
		floatMetric: {kind: metrics.KindFloat64, float64: 1.5},
	})
	current := testSnapshot(time.Unix(12, 0), map[string]Value{
		uintMetric:  {kind: metrics.KindUint64, uint64: 17},
		floatMetric: {kind: metrics.KindFloat64, float64: 4.5},
	})

	report := CheckRate(previous, current, map[string]float64{
		uintMetric:  3,
		floatMetric: 1,
	})
	if len(report.Violations) != 2 || report.Violations[0].Name != floatMetric || report.Violations[0].Value != 1.5 || report.Violations[1].Name != uintMetric || report.Violations[1].Value != 3.5 {
		t.Fatalf("rate violations = %#v", report.Violations)
	}
	if report.Violations[0].Kind != metrics.KindFloat64 || report.Violations[1].Kind != metrics.KindUint64 {
		t.Fatalf("rate violation kinds = %#v", report.Violations)
	}

	negative := CheckRate(previous, current, map[string]float64{floatMetric: -1})
	if len(negative.Violations) != 1 || negative.Violations[0].Limit != -1 {
		t.Fatalf("negative rate budget = %#v", negative)
	}
	if !errors.Is(CheckRate(current, previous, map[string]float64{floatMetric: 1}).Err(), ErrInvalidInterval) {
		t.Fatal("invalid interval was not reported by CheckRate")
	}
}

func TestWatchRateValidation(t *testing.T) {
	callback := func(Report) {}
	if !errors.Is(WatchRate(nil, time.Second, nil, callback), ErrNilContext) {
		t.Fatal("nil context was not rejected")
	}
	if !errors.Is(WatchRate(context.Background(), time.Second, nil, nil), ErrNilCallback) {
		t.Fatal("nil callback was not rejected")
	}
	if !errors.Is(WatchRate(context.Background(), 0, nil, callback), ErrInvalidWatchInterval) {
		t.Fatal("non-positive interval was not rejected")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(WatchRate(ctx, time.Second, nil, callback), context.Canceled) {
		t.Fatal("canceled context was not returned")
	}
}

func TestWatchRateSamplesSelectedMetricsInOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	budgets := map[string]float64{uintMetric: 0, floatMetric: 0}
	samples := []Snapshot{
		testSnapshot(time.Unix(1, 0), map[string]Value{
			uintMetric:  {kind: metrics.KindUint64, uint64: 0},
			floatMetric: {kind: metrics.KindFloat64, float64: 0},
		}),
		testSnapshot(time.Unix(2, 0), map[string]Value{
			uintMetric:  {kind: metrics.KindUint64, uint64: 2},
			floatMetric: {kind: metrics.KindFloat64, float64: 2},
		}),
		testSnapshot(time.Unix(3, 0), map[string]Value{
			uintMetric:  {kind: metrics.KindUint64, uint64: 4},
			floatMetric: {kind: metrics.KindFloat64, float64: 4},
		}),
	}
	var reads int
	var readNames [][]string
	var events []string
	read := func(names []string) Snapshot {
		events = append(events, "read")
		readNames = append(readNames, append([]string(nil), names...))
		snapshot := samples[reads]
		reads++
		return snapshot
	}
	var reports []Report
	onReport := func(report Report) {
		events = append(events, "callback")
		if len(reports) == 0 {
			if len(report.Violations) != 2 {
				t.Fatalf("first report violations = %#v", report.Violations)
			}
			report.Violations[0].Name = "changed by callback"
		}
		reports = append(reports, report)
		if len(reports) == 2 {
			cancel()
		}
	}

	err := watchRate(ctx, time.Millisecond, budgets, budgetNames(budgets), onReport, read)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WatchRate error = %v, want context.Canceled", err)
	}
	if reads != 3 || len(reports) != 2 {
		t.Fatalf("reads/reports = %d/%d, want 3/2", reads, len(reports))
	}
	wantEvents := []string{"read", "read", "callback", "read", "callback"}
	if len(events) != len(wantEvents) {
		t.Fatalf("sampling events = %#v, want %#v", events, wantEvents)
	}
	for i := range wantEvents {
		if events[i] != wantEvents[i] {
			t.Fatalf("sampling events = %#v, want %#v", events, wantEvents)
		}
	}
	wantNames := []string{floatMetric, uintMetric}
	for i, names := range readNames {
		if len(names) != len(wantNames) || names[0] != wantNames[0] || names[1] != wantNames[1] {
			t.Fatalf("read %d names = %#v, want %#v", i, names, wantNames)
		}
	}
	if reports[1].Violations[0].Name != floatMetric {
		t.Fatalf("second report was affected by callback mutation: %#v", reports[1])
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
