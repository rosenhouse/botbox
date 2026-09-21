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

// sameSecond is how many invocations of one seed can open a directory in the
// same second before botbox gives up naming them apart.
const sameSecond = 100

// OpenOutput creates the invocation's directory under root. A second
// invocation of the same seed in the same second takes the next name rather
// than the same directory: two runs' recordings in one directory would leave
// each reader pointed at the other's evidence.
func OpenOutput(root string, seed int64, at time.Time) (*Output, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("creating the output directory: %w", err)
	}
	named := filepath.Join(root, fmt.Sprintf("%s-%d", at.UTC().Format(stamp), seed))
	for n := 1; n <= sameSecond; n++ {
		dir := named
		if n > 1 {
			dir = fmt.Sprintf("%s-%d", named, n)
		}
		switch err := os.Mkdir(dir, 0o755); {
		case err == nil:
			return &Output{dir: dir}, nil
		case !os.IsExist(err):
			return nil, fmt.Errorf("creating the output directory: %w", err)
		}
	}
	return nil, fmt.Errorf("creating the output directory: %s and its next %d names are taken", named, sameSecond-1)
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
