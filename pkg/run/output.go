package run

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// stamp names an invocation's directory in a form that sorts by time.
const stamp = "20060102T150405Z"

// Output is one invocation's directory tree: <out>/<timestamp>-<seed>/, with a
// run-<n>/ per run. Passing runs are not persisted (DESIGN.md §11).
type Output struct{ dir string }

// OpenOutput creates the invocation's directory under root.
func OpenOutput(root string, seed int64, at time.Time) (*Output, error) {
	dir := filepath.Join(root, fmt.Sprintf("%s-%d", at.UTC().Format(stamp), seed))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating the output directory: %w", err)
	}
	return &Output{dir: dir}, nil
}

// Dir is the invocation's directory.
func (o *Output) Dir() string { return o.dir }

// RunDir is where run n writes its recordings.
func (o *Output) RunDir(n int) string { return filepath.Join(o.dir, fmt.Sprintf("run-%d", n)) }

// Discard removes a passing run's directory.
func (o *Output) Discard(n int) error {
	if err := os.RemoveAll(o.RunDir(n)); err != nil {
		return fmt.Errorf("discarding the passing run: %w", err)
	}
	return nil
}
