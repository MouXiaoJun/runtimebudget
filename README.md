# runtimebudget

`runtimebudget` is a Go 1.23, standard-library-only package for reading and comparing `runtime/metrics` snapshots.

```go
const name = "/gc/cycles/total:gc-cycles"

before := runtimebudget.Read(name)
// ... work ...
after := runtimebudget.Read(name)

delta, err := runtimebudget.Delta(before, after, name)
if err != nil {
	return err
}
count, ok := delta.Uint64()
_ = count
_ = ok

rate, err := runtimebudget.Rate(before, after, name)
```

`Read()` and `ReadAll()` read all metrics; passing names reads only those metrics. A name unknown to the current Go runtime is retained as `KindBad`. `Snapshot.Get` also distinguishes a missing name from a present `KindBad` value. `Value.Uint64`, `Value.Float64`, and `Value.Histogram` return `(value, false)` instead of panicking for the wrong kind.

`Delta` and `Rate` only accept cumulative scalar metrics, as declared by `runtime/metrics.Description.Cumulative`. A counter decrease is reported as `ErrCounterReset`. Histograms are never converted to a scalar: use `HistogramDelta` for per-bucket count differences. Divide those counts by `current.At().Sub(previous.At()).Seconds()` when a bucket rate is needed.

For non-cumulative scalar metrics, check maximum budgets:

```go
report := runtimebudget.Check(snapshot, map[string]float64{
	"/gc/heap/goal:bytes": 64 << 20,
})
if err := report.Err(); err != nil {
	// errors.Is(err, runtimebudget.ErrBudgetExceeded) identifies violations.
	// report.Violations contains their values and limits; report.Issues contains
	// unknown, missing, KindBad, histogram, and other unevaluable metrics.
	return err
}
```

For cumulative budgets, compare two snapshots directly or per second:

```go
report := runtimebudget.CheckDelta(before, after, map[string]float64{
	"/gc/cycles/total:gc-cycles": 100,
})
// CheckRate(before, after, budgets) uses the same budgets as values/second.
if err := report.Err(); err != nil {
	return err
}
```

To keep checking a cumulative budget without managing snapshots yourself:

```go
err := runtimebudget.WatchRate(ctx, time.Second, map[string]float64{
	"/gc/cycles/total:gc-cycles": 100,
}, func(report runtimebudget.Report) {
		if err := report.Err(); err != nil {
			// Handle a violation or an unevaluable metric.
		}
})
// err is ctx.Err() after cancellation.
```

`WatchRate` samples the selected metrics immediately, then once per interval.
Its callback runs synchronously; pass a non-nil context, callback, and positive
interval.

The runtime metrics API is implementation-defined and evolves with Go. This package consults `runtime/metrics.All()` at runtime rather than maintaining its own metric list.

API reference: [runtime/metrics package](https://pkg.go.dev/runtime/metrics), [Go source](https://go.dev/src/runtime/metrics/doc.go).
