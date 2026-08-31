package runtimebudget_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/MouXiaoJun/runtimebudget"
)

// Measure an isolated batch's process-wide allocation growth. Do not run this
// alongside unrelated work and interpret the result as request-local usage.
func ExampleCheckDelta() {
	const metric = "/gc/heap/allocs:bytes"
	batch := []struct {
		ID   int
		Body string
	}{{ID: 1, Body: strings.Repeat("x", 64<<10)}}
	runtime.GC()
	before := runtimebudget.Read(metric)

	encoded, err := json.Marshal(batch)
	if err != nil {
		panic(err)
	}
	// GC flushes runtime allocation accounting for this repeatable example.
	// Do not force GC per request in a production service.
	runtime.GC()
	after := runtimebudget.Read(metric)
	runtime.KeepAlive(encoded)

	// Illustrative thresholds, not recommended production limits.
	report := runtimebudget.CheckDelta(before, after, map[string]float64{metric: 8 << 20})
	if err := report.Err(); err != nil {
		panic(err) // An unknown/unavailable metric is also a failure, never a pass.
	}
	fmt.Println("batch within allocation budget:", report.OK())

	tooSmall := runtimebudget.CheckDelta(before, after, map[string]float64{metric: 1})
	if len(tooSmall.Issues) != 0 {
		panic(tooSmall.Err())
	}
	fmt.Println("one-byte budget exceeded:", errors.Is(tooSmall.Err(), runtimebudget.ErrBudgetExceeded))
	// Output:
	// batch within allocation budget: true
	// one-byte budget exceeded: true
}
