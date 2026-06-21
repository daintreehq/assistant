//go:build race

package e2e

// raceEnabled is true when built with the race detector. The binary-spawning
// e2e tests build + run a separate, non-instrumented process, so they add no
// race coverage and only flake under `go test -race ./...` load; they skip
// themselves when this is true.
const raceEnabled = true
