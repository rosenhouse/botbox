// Command botbox exercises a Kubernetes controller against the generic
// invariants of DESIGN.md §6.
package main

import (
	"context"
	"os"
)

func main() {
	// pkg/generate's rapid-driven generator replaces sampleGenerator here
	// (DESIGN.md §5.4).
	c := &cli{stdout: os.Stdout, stderr: os.Stderr, open: openSession, newGenerator: sampleGenerator}
	os.Exit(c.main(context.Background(), os.Args[1:]))
}
