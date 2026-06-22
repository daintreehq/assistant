package e2e

// pty_deps_test.go anchors github.com/creack/pty in the module graph. The only
// real use of the dependency is the PTY render harness (pty_test.go), which sits
// behind `//go:build pty` — a tag NOT in the default build set, so without this
// no-tag blank import `go mod tidy` would prune creack/pty from go.mod and the
// `make test-pty` build would break. A test-file import keeps it test-scoped (it
// never reaches the production binary). creack/pty is CGO-free, so importing it
// here does not disturb the CGO_ENABLED=0 build of `go test ./...`.
import _ "github.com/creack/pty"
