// Package runtimebudget reads runtime/metrics snapshots and compares them.
package runtimebudget

import (
	"errors"
	"fmt"
	"math"
	"runtime/metrics"
	"sort"
	"strings"
	"time"
)

var (
	// ErrUnknownMetric means the metric name is not in runtime/metrics.All.
	ErrUnknownMetric = errors.New("runtimebudget: unknown metric")
	// ErrMetricMissing means a snapshot does not contain the requested metric.
	ErrMetricMissing = errors.New("runtimebudget: metric missing from snapshot")
	// ErrMetricUnavailable means runtime/metrics returned KindBad.
	ErrMetricUnavailable = errors.New("runtimebudget: metric unavailable")
	// ErrKindMismatch means a value's kind differs from its runtime description.
	ErrKindMismatch = errors.New("runtimebudget: metric kind mismatch")
	// ErrUnsupportedKind means a newer or otherwise unsupported metric kind was seen.
	ErrUnsupportedKind = errors.New("runtimebudget: unsupported metric kind")
	// ErrNotCumulative means an operation requiring a cumulative metric was requested.
	ErrNotCumulative = errors.New("runtimebudget: metric is not cumulative")
	// ErrHistogramScalarDelta means a histogram was passed to scalar Delta or Rate.
	ErrHistogramScalarDelta = errors.New("runtimebudget: histogram needs bucket handling")
	// ErrHistogramRequired means HistogramDelta was passed a non-histogram metric.
	ErrHistogramRequired = errors.New("runtimebudget: metric is not a histogram")
	// ErrHistogramLayout means two histogram snapshots have different bucket boundaries.
	ErrHistogramLayout = errors.New("runtimebudget: histogram bucket layout mismatch")
	// ErrCounterReset means a cumulative value decreased between snapshots.
	ErrCounterReset = errors.New("runtimebudget: cumulative metric decreased")
	// ErrInvalidInterval means the current snapshot is not after the previous one.
	ErrInvalidInterval = errors.New("runtimebudget: snapshot interval must be positive")
	// ErrInvalidBudget means a budget limit is NaN or infinite.
	ErrInvalidBudget = errors.New("runtimebudget: budget limit must be finite")
	// ErrHistogramBudget means a histogram cannot be checked as one scalar value.
	ErrHistogramBudget = errors.New("runtimebudget: histogram budget needs bucket handling")
	// ErrBudgetExceeded means at least one budget was exceeded.
	ErrBudgetExceeded = errors.New("runtimebudget: budget exceeded")
)

// Snapshot is an immutable copy of selected runtime metrics at one point in time.
// The snapshot's histogram slices are copied, so a later runtime/metrics.Read does
// not change this snapshot.
type Snapshot struct {
	at     time.Time
	values map[string]Value
}

// Read reads the named metrics. With no names, it reads every metric currently
// returned by runtime/metrics.All. Unknown names are retained with KindBad.
func Read(names ...string) Snapshot {
	if len(names) == 0 {
		descriptions := metrics.All()
		names = make([]string, len(descriptions))
		for i, description := range descriptions {
			names[i] = description.Name
		}
	}

	samples := make([]metrics.Sample, len(names))
	for i, name := range names {
		samples[i].Name = name
	}
	metrics.Read(samples)

	values := make(map[string]Value, len(samples))
	for _, sample := range samples {
		values[sample.Name] = copyValue(sample.Value)
	}
	return Snapshot{at: time.Now(), values: values}
}

// ReadAll reads every metric currently returned by runtime/metrics.All.
func ReadAll() Snapshot { return Read() }

// At reports when the snapshot was captured.
func (s Snapshot) At() time.Time { return s.at }

// Get returns a metric value and whether the snapshot contains that name.
// A present but unsupported or unknown runtime metric has KindBad and still
// returns true.
func (s Snapshot) Get(name string) (Value, bool) {
	value, ok := s.values[name]
	return value, ok
}

// Names returns the metric names in the snapshot in sorted order.
func (s Snapshot) Names() []string {
	names := make([]string, 0, len(s.values))
	for name := range s.values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Value is a kind-tagged runtime metric value.
type Value struct {
	kind      metrics.ValueKind
	uint64    uint64
	float64   float64
	histogram Histogram
}

// Kind reports the runtime/metrics kind of the value.
func (v Value) Kind() metrics.ValueKind { return v.kind }

// Uint64 returns the value when its kind is KindUint64.
func (v Value) Uint64() (uint64, bool) {
	return v.uint64, v.kind == metrics.KindUint64
}

// Float64 returns the value when its kind is KindFloat64.
func (v Value) Float64() (float64, bool) {
	return v.float64, v.kind == metrics.KindFloat64
}

// Histogram returns a copy when its kind is KindFloat64Histogram.
func (v Value) Histogram() (Histogram, bool) {
	if v.kind != metrics.KindFloat64Histogram {
		return Histogram{}, false
	}
	return cloneHistogram(v.histogram), true
}

// Histogram is an explicit bucketed metric value. Counts[i] covers
// [Buckets[i], Buckets[i+1]).
type Histogram struct {
	Counts  []uint64
	Buckets []float64
}

// Delta returns the scalar increase of a cumulative metric. Histograms are
// intentionally rejected; use HistogramDelta for their bucket counts.
func Delta(previous, current Snapshot, name string) (Value, error) {
	description, err := cumulativeDescription(name)
	if err != nil {
		return Value{}, err
	}
	previousValue, err := snapshotValue(previous, description, name)
	if err != nil {
		return Value{}, err
	}
	currentValue, err := snapshotValue(current, description, name)
	if err != nil {
		return Value{}, err
	}
	if previousValue.kind == metrics.KindFloat64Histogram {
		return Value{}, metricError(ErrHistogramScalarDelta, name)
	}

	switch description.Kind {
	case metrics.KindUint64:
		if currentValue.uint64 < previousValue.uint64 {
			return Value{}, metricError(ErrCounterReset, name)
		}
		return Value{kind: metrics.KindUint64, uint64: currentValue.uint64 - previousValue.uint64}, nil
	case metrics.KindFloat64:
		if currentValue.float64 < previousValue.float64 {
			return Value{}, metricError(ErrCounterReset, name)
		}
		return Value{kind: metrics.KindFloat64, float64: currentValue.float64 - previousValue.float64}, nil
	case metrics.KindFloat64Histogram:
		return Value{}, metricError(ErrHistogramScalarDelta, name)
	default:
		return Value{}, metricError(ErrUnsupportedKind, name)
	}
}

// Rate returns the scalar cumulative increase per second, using the timestamps
// captured in the two snapshots. Histograms are intentionally rejected.
func Rate(previous, current Snapshot, name string) (float64, error) {
	elapsed := current.at.Sub(previous.at)
	if elapsed <= 0 {
		return 0, fmt.Errorf("%w: %s", ErrInvalidInterval, elapsed)
	}
	delta, err := Delta(previous, current, name)
	if err != nil {
		return 0, err
	}
	seconds := elapsed.Seconds()
	switch delta.kind {
	case metrics.KindUint64:
		return float64(delta.uint64) / seconds, nil
	case metrics.KindFloat64:
		return delta.float64 / seconds, nil
	default:
		return 0, metricError(ErrUnsupportedKind, name)
	}
}

// HistogramDelta returns per-bucket increases for a cumulative histogram.
// Buckets are copied from the current snapshot.
func HistogramDelta(previous, current Snapshot, name string) (Histogram, error) {
	description, err := cumulativeDescription(name)
	if err != nil {
		return Histogram{}, err
	}
	if description.Kind != metrics.KindFloat64Histogram {
		return Histogram{}, metricError(ErrHistogramRequired, name)
	}
	previousValue, err := snapshotValue(previous, description, name)
	if err != nil {
		return Histogram{}, err
	}
	currentValue, err := snapshotValue(current, description, name)
	if err != nil {
		return Histogram{}, err
	}
	if !sameBuckets(previousValue.histogram.Buckets, currentValue.histogram.Buckets) ||
		len(previousValue.histogram.Counts) != len(currentValue.histogram.Counts) {
		return Histogram{}, metricError(ErrHistogramLayout, name)
	}

	counts := make([]uint64, len(currentValue.histogram.Counts))
	for i, count := range currentValue.histogram.Counts {
		if count < previousValue.histogram.Counts[i] {
			return Histogram{}, metricError(ErrCounterReset, name)
		}
		counts[i] = count - previousValue.histogram.Counts[i]
	}
	return Histogram{Counts: counts, Buckets: append([]float64(nil), currentValue.histogram.Buckets...)}, nil
}

// Violation describes one numeric value above its configured maximum.
type Violation struct {
	Name  string
	Value float64
	Limit float64
	Kind  metrics.ValueKind
}

func (v Violation) Error() string {
	return fmt.Sprintf("%q=%g exceeds budget %g", v.Name, v.Value, v.Limit)
}

// Issue describes a budget that could not be evaluated.
type Issue struct {
	Name string
	Err  error
}

func (i Issue) Error() string { return fmt.Sprintf("%q: %v", i.Name, i.Err) }

// Report contains budget violations and metrics that could not be checked.
type Report struct {
	Violations []Violation
	Issues     []Issue
}

// OK reports whether every budget was evaluated and satisfied.
func (r Report) OK() bool { return len(r.Violations) == 0 && len(r.Issues) == 0 }

// Err converts a non-OK report into a clear, inspectable error.
func (r Report) Err() error {
	if r.OK() {
		return nil
	}
	return &ReportError{Report: r}
}

// ReportError is returned by Report.Err when a check has violations or issues.
type ReportError struct {
	Report Report
}

func (e *ReportError) Error() string {
	parts := make([]string, 0, 2)
	if len(e.Report.Violations) != 0 {
		violations := make([]string, len(e.Report.Violations))
		for i, violation := range e.Report.Violations {
			violations[i] = violation.Error()
		}
		parts = append(parts, "violations: "+strings.Join(violations, "; "))
	}
	if len(e.Report.Issues) != 0 {
		issues := make([]string, len(e.Report.Issues))
		for i, issue := range e.Report.Issues {
			issues[i] = issue.Error()
		}
		parts = append(parts, "issues: "+strings.Join(issues, "; "))
	}
	return "runtimebudget: " + strings.Join(parts, "; ")
}

// Unwrap makes errors.Is useful for both violations and evaluation issues.
func (e *ReportError) Unwrap() []error {
	errs := make([]error, 0, 1+len(e.Report.Issues))
	if len(e.Report.Violations) != 0 {
		errs = append(errs, ErrBudgetExceeded)
	}
	for _, issue := range e.Report.Issues {
		errs = append(errs, issue.Err)
	}
	return errs
}

// Check evaluates maximum numeric budgets for non-cumulative scalar metrics.
// The returned report is deterministic by metric name. Use report.Err() when
// the caller wants violations and evaluation issues as an error.
func Check(snapshot Snapshot, budgets map[string]float64) Report {
	var report Report
	names := make([]string, 0, len(budgets))
	for name := range budgets {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		limit := budgets[name]
		if math.IsNaN(limit) || math.IsInf(limit, 0) {
			report.Issues = append(report.Issues, Issue{Name: name, Err: metricError(ErrInvalidBudget, name)})
			continue
		}

		description, ok := findDescription(name)
		if !ok {
			report.Issues = append(report.Issues, Issue{Name: name, Err: metricError(ErrUnknownMetric, name)})
			continue
		}
		if description.Cumulative {
			report.Issues = append(report.Issues, Issue{Name: name, Err: metricError(ErrNotCumulative, name)})
			continue
		}

		value, err := snapshotValue(snapshot, description, name)
		if err != nil {
			report.Issues = append(report.Issues, Issue{Name: name, Err: err})
			continue
		}
		switch value.kind {
		case metrics.KindUint64:
			if uint64Exceeds(value.uint64, limit) {
				report.Violations = append(report.Violations, Violation{Name: name, Value: float64(value.uint64), Limit: limit, Kind: value.kind})
			}
		case metrics.KindFloat64:
			if value.float64 > limit {
				report.Violations = append(report.Violations, Violation{Name: name, Value: value.float64, Limit: limit, Kind: value.kind})
			}
		case metrics.KindFloat64Histogram:
			report.Issues = append(report.Issues, Issue{Name: name, Err: metricError(ErrHistogramBudget, name)})
		default:
			report.Issues = append(report.Issues, Issue{Name: name, Err: metricError(ErrUnsupportedKind, name)})
		}
	}
	return report
}

func copyValue(value metrics.Value) Value {
	result := Value{kind: value.Kind()}
	switch result.kind {
	case metrics.KindUint64:
		result.uint64 = value.Uint64()
	case metrics.KindFloat64:
		result.float64 = value.Float64()
	case metrics.KindFloat64Histogram:
		histogram := value.Float64Histogram()
		if histogram != nil {
			result.histogram = Histogram{
				Counts:  append([]uint64(nil), histogram.Counts...),
				Buckets: append([]float64(nil), histogram.Buckets...),
			}
		}
	}
	return result
}

func findDescription(name string) (metrics.Description, bool) {
	for _, description := range metrics.All() {
		if description.Name == name {
			return description, true
		}
	}
	return metrics.Description{}, false
}

func cumulativeDescription(name string) (metrics.Description, error) {
	description, ok := findDescription(name)
	if !ok {
		return metrics.Description{}, metricError(ErrUnknownMetric, name)
	}
	if !description.Cumulative {
		return metrics.Description{}, metricError(ErrNotCumulative, name)
	}
	return description, nil
}

func snapshotValue(snapshot Snapshot, description metrics.Description, name string) (Value, error) {
	value, ok := snapshot.Get(name)
	if !ok {
		return Value{}, metricError(ErrMetricMissing, name)
	}
	if value.kind == metrics.KindBad {
		return Value{}, metricError(ErrMetricUnavailable, name)
	}
	if value.kind != description.Kind {
		return Value{}, fmt.Errorf("%w: %q: got %v, want %v", ErrKindMismatch, name, value.kind, description.Kind)
	}
	if value.kind != metrics.KindUint64 && value.kind != metrics.KindFloat64 && value.kind != metrics.KindFloat64Histogram {
		return Value{}, metricError(ErrUnsupportedKind, name)
	}
	return value, nil
}

func metricError(kind error, name string) error { return fmt.Errorf("%w: %q", kind, name) }

func cloneHistogram(histogram Histogram) Histogram {
	return Histogram{
		Counts:  append([]uint64(nil), histogram.Counts...),
		Buckets: append([]float64(nil), histogram.Buckets...),
	}
}

func sameBuckets(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func uint64Exceeds(value uint64, limit float64) bool {
	if limit < 0 {
		return true
	}
	if limit >= float64(^uint64(0)) {
		return false
	}
	return value > uint64(limit)
}
