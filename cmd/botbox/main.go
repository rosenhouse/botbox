// Command botbox exercises a Kubernetes controller against the generic
// invariants of DESIGN.md §6.
package main

import (
	"context"
	"os"
)

func main() {
	c := &cli{stdout: os.Stdout, stderr: os.Stderr, open: openSession, newGenerator: rapidGenerator}
	os.Exit(c.main(context.Background(), os.Args[1:]))
}
