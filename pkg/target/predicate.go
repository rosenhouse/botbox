package target

import (
	"errors"
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ErrNotBool marks a predicate whose result is not a bool. DESIGN.md §8.4
// makes that a configuration error, never "not ready".
var ErrNotBool = errors.New("the expression did not yield a bool")

// EvalError reports a predicate that failed to evaluate, so that a report can
// quote it (DESIGN.md §8.4).
type EvalError struct {
	Predicate string // "ready", or the property ID
	Expr      string
	Err       error
}

func (e *EvalError) Error() string {
	return fmt.Sprintf("evaluating %s %q: %v", e.Predicate, e.Expr, e.Err)
}

func (e *EvalError) Unwrap() error { return e.Err }

// celEnv binds metadata, spec and status, plus whatever extra a property needs
// (DESIGN.md §8.4).
func celEnv(extra ...cel.EnvOption) (*cel.Env, error) {
	opts := []cel.EnvOption{
		cel.Variable("metadata", cel.DynType),
		cel.Variable("spec", cel.DynType),
		cel.Variable("status", cel.DynType),
		ext.Strings(),
	}
	return cel.NewEnv(append(opts, extra...)...)
}

func compileReady(expr string) (ReadyFunc, error) {
	env, err := celEnv()
	if err != nil {
		return nil, err
	}
	program, err := compile(env, expr)
	if err != nil {
		return nil, err
	}
	return func(cr *unstructured.Unstructured) (bool, error) {
		return evalBool(program, crVars(cr), "ready", expr)
	}, nil
}

func compileProperty(id, expr string) (PropertyFunc, error) {
	env, err := celEnv(cel.Variable("managed", cel.ListType(cel.DynType)))
	if err != nil {
		return nil, err
	}
	program, err := compile(env, expr)
	if err != nil {
		return nil, err
	}
	return func(cr *unstructured.Unstructured, managed []*unstructured.Unstructured) (bool, error) {
		vars := crVars(cr)
		objects := make([]any, len(managed))
		for i, object := range managed {
			objects[i] = object.Object
		}
		vars["managed"] = objects
		return evalBool(program, vars, id, expr)
	}, nil
}

func compile(env *cel.Env, expr string) (cel.Program, error) {
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	if out := ast.OutputType(); !out.IsExactType(cel.BoolType) && !out.IsExactType(cel.DynType) {
		return nil, fmt.Errorf("expression yields %s, want bool", out)
	}
	return env.Program(ast)
}

func evalBool(program cel.Program, vars map[string]any, predicate, expr string) (bool, error) {
	out, _, err := program.Eval(vars)
	if err != nil {
		return false, &EvalError{Predicate: predicate, Expr: expr, Err: err}
	}
	value, isBool := out.Value().(bool)
	if !isBool {
		return false, &EvalError{Predicate: predicate, Expr: expr, Err: fmt.Errorf("%w: it yielded %T", ErrNotBool, out.Value())}
	}
	return value, nil
}

func crVars(cr *unstructured.Unstructured) map[string]any {
	object := map[string]any{}
	if cr != nil {
		object = cr.Object
	}
	return map[string]any{
		"metadata": dynMap(object, "metadata"),
		"spec":     dynMap(object, "spec"),
		"status":   dynMap(object, "status"),
	}
}

// dynMap binds one top-level field. A missing field binds to an empty map, so
// that an expression guarded with has() still evaluates (DESIGN.md §8.4).
func dynMap(object map[string]any, field string) map[string]any {
	value, isMap := object[field].(map[string]any)
	if !isMap {
		return map[string]any{}
	}
	return value
}
