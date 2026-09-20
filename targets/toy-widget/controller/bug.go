package controller

import "fmt"

// MaxBug is the highest seeded bug ID in the catalog (DESIGN.md §9.1).
const MaxBug = 10

// Bug selects the seeded bug the controller runs with. Zero is the correct
// controller; the behaviour of B1 through B10 hooks in here.
type Bug int

// ParseBug converts a --bug value into a Bug.
func ParseBug(id int) (Bug, error) {
	if id < 0 || id > MaxBug {
		return 0, fmt.Errorf("--bug=%d: want a bug ID from 0 to %d", id, MaxBug)
	}
	return Bug(id), nil
}
