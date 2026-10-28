// Package conformance holds differential tests that drive the canonical
// Python servers (Lisa-Harness/lisa) and the Go ports with identical MCP
// calls over stdio and compare the observable results.
//
// The tests need a Python interpreter with the canonical lisa workspace
// installed, passed via LISA_CONFORMANCE_PYTHON. Without it they skip, unless
// LISA_CONFORMANCE_REQUIRED=1, in which case they fail: CI must never report
// a skipped conformance run as green. Run them via scripts/conformance.sh.
package conformance
