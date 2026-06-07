// This file was created by `orchestrion pin`, and is used to ensure the
// `go.mod` file contains the necessary entries to ensure repeatable builds when
// using `orchestrion`. It is also used to set up which integrations are enabled.

//go:build tools

//go:generate go run github.com/DataDog/orchestrion pin -generate

package tools

// Standard `orchestrion pin` output: the imports here decide which integrations orchestrion enables.
//
// Our own code uses none of them, though. No franz-go, no kafka, no http.
// The leak is in the core tracer GLS (ContextWithSpan push, Finish pop).
// The `all` import below is just orchestrion's bootstrap; it's what turns the core GLS on at build time.
// Take it out, and the GLS is off, and the leak goes away, which is one more sign the bug is in the core tracer and not a contrib.
import (
	// Ensures `orchestrion` is present in `go.mod` so that builds are repeatable.
	// Do not remove.
	_ "github.com/DataDog/orchestrion" // integration

	_ "github.com/DataDog/dd-trace-go/orchestrion/all/v2" // integration
)
