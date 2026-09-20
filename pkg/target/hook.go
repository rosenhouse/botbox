package target

import (
	"fmt"
	"strings"
	"sync"
)

// hookPrefix marks a predicate that names a Go hook instead of CEL, as
// `ready: go:<name>` or `equal: go:<name>` (DESIGN.md §8.4).
const hookPrefix = "go:"

var hooks = struct {
	sync.Mutex
	ready map[string]ReadyFunc
	equal map[string]EqualFunc
}{
	ready: map[string]ReadyFunc{},
	equal: map[string]EqualFunc{},
}

// RegisterReady registers fn for `ready: go:<name>`. It panics on a duplicate
// name. Hooks exist for in-repo targets only.
func RegisterReady(name string, fn ReadyFunc) {
	hooks.Lock()
	defer hooks.Unlock()
	if _, taken := hooks.ready[name]; taken {
		panic(fmt.Sprintf("ready hook %q is already registered", name))
	}
	hooks.ready[name] = fn
}

// RegisterEqual registers fn for `equal: go:<name>`. It panics on a duplicate
// name.
func RegisterEqual(name string, fn EqualFunc) {
	hooks.Lock()
	defer hooks.Unlock()
	if _, taken := hooks.equal[name]; taken {
		panic(fmt.Sprintf("equal hook %q is already registered", name))
	}
	hooks.equal[name] = fn
}

func readyHook(name string) (ReadyFunc, error) {
	hooks.Lock()
	defer hooks.Unlock()
	fn, found := hooks.ready[name]
	if !found {
		return nil, fmt.Errorf("no ready hook named %q is registered", name)
	}
	return fn, nil
}

func equalHook(name string) (EqualFunc, error) {
	hooks.Lock()
	defer hooks.Unlock()
	fn, found := hooks.equal[name]
	if !found {
		return nil, fmt.Errorf("no equal hook named %q is registered", name)
	}
	return fn, nil
}

// hookName returns the name in `go:<name>`, and whether the field names a hook
// at all.
func hookName(field string) (string, bool) {
	name, isHook := strings.CutPrefix(field, hookPrefix)
	return name, isHook
}
