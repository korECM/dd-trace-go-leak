// Command gls-leak reproduces the GLS span leak that dd-trace-go v2 hits in Orchestrion builds, with no contrib involved.
//
// ContextWithSpan pushes a span onto the calling goroutine's GLS stack, and Finish pops the stack of whichever goroutine runs it.
// Push on one goroutine, finish on another, and the push is never popped: one span leaks per call.
//
// See README.md for how to run it under Orchestrion.
package main

import (
	"context"
	"flag"
	"fmt"
	"runtime"

	"github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
)

func main() {
	n := flag.Int("n", 200_000, "number of records to simulate")
	flag.Parse()

	// Start the tracer so StartSpan hands back real *Span objects.
	// No Agent needed; spans that can't be flushed just get dropped.
	if err := tracer.Start(tracer.WithLogStartup(false)); err != nil {
		panic(err)
	}
	defer tracer.Stop()

	// Stand-in for the per-record handler context.
	// We reuse the same base and throw the result away, so the only thing that can grow across iterations is the GLS stack, not a context.WithValue chain.
	baseCtx := context.Background()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	goroutinesBefore := runtime.NumGoroutine()

	// Owner goroutine: this is what franz-go's tracing hook does in real life.
	// It creates each span and finishes it on its own goroutine; the worker never owns them.
	spanCh := make(chan *tracer.Span, 1024)
	go func() {
		for range *n {
			s := tracer.StartSpan("kafka.consume")
			spanCh <- s
			s.Finish() // pop runs here, on the owner goroutine, not the worker
		}
		close(spanCh)
	}()

	// Worker goroutine: re-inject each span into the handler context, the way a Kafka consumer makes its processing a child of the consume span:
	//
	//     span, _ := tracer.SpanFromContext(record.Context)
	//     ctx = tracer.ContextWithSpan(ctx, span)
	//
	// Under Orchestrion that ContextWithSpan pushes onto the worker's GLS stack.
	// The span is finished on the owner goroutine, so the push is never popped: one span leaks per record.
	for s := range spanCh {
		_ = tracer.ContextWithSpan(baseCtx, s)
	}

	// Flush dropped spans out of the tracer buffers, then let the heap settle.
	tracer.Flush()
	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	goroutinesAfter := runtime.NumGoroutine()

	liveObjects := int64(after.HeapObjects) - int64(before.HeapObjects)
	liveBytes := int64(after.HeapInuse) - int64(before.HeapInuse)

	fmt.Printf("records simulated        : %d\n", *n)
	fmt.Printf("goroutines (before/after): %d / %d\n", goroutinesBefore, goroutinesAfter)
	fmt.Printf("retained heap objects    : %+d  (%.3f per record)\n", liveObjects, float64(liveObjects)/float64(*n))
	fmt.Printf("retained heap bytes      : %+d  (%.1f KiB)\n", liveBytes, float64(liveBytes)/1024)
	fmt.Println()
	fmt.Println("Interpretation:")
	fmt.Println("  plain `go run .`   -> ~0 retained per record (GLS disabled, nothing leaks)")
	fmt.Println("  orchestrion build  -> ~1 span retained per record (GLS push never popped)")
}
