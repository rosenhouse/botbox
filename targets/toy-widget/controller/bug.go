package controller

import (
	"fmt"
	"time"
)

// Bug selects the seeded bug the controller runs with. Zero is the correct
// controller; B1 through B12 are the seeded bugs.
type Bug int

const (
	B1 Bug = iota + 1
	B2
	B3
	B4
	B5
	B6
	B7
	B8
	B9
	B10
	B11
	B12
)

// MaxBug is the highest seeded bug ID in the catalog.
const MaxBug = int(B12)

// defaultB1Hold is Reconciler.B1Hold's value when left unset.
const defaultB1Hold = 3 * time.Second

// ParseBug converts a --bug value into a Bug.
func ParseBug(id int) (Bug, error) {
	if id < 0 || id > MaxBug {
		return 0, fmt.Errorf("--bug=%d: want a bug ID from 0 to %d", id, MaxBug)
	}
	return Bug(id), nil
}
