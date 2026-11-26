// Package pssgate is the load for the CI PSS memory gate (scripts/dev/pss-gate.sh).
//
// The load lives in load_test.go behind the build tag pssgate, so a normal
// `go test ./...` never runs it:
//
//	go test -c -tags pssgate -o pssload ./internal/pssgate
//
// pss-gate.sh starts a real Moorage process, runs this load against it (15 agents paired
// over the tunnel, 100 HTTP checks, two clients streaming logs), lets it settle and reads
// the Moorage process's /proc/<pid>/smaps_rollup. Only Moorage is measured, never the load.
package pssgate
