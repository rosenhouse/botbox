package run

// mergePatch applies a JSON merge patch (RFC 7386) to object, in place, and
// returns it. A null in the patch removes the field; an object merges into an
// object and replaces anything else; every other value replaces. The patch is
// left as it was, and the object takes the patch's values without copying
// them.
func mergePatch(object, patch map[string]any) map[string]any {
	if object == nil {
		object = map[string]any{}
	}
	for field, value := range patch {
		nested, isObject := value.(map[string]any)
		switch {
		case value == nil:
			delete(object, field)
		case !isObject:
			object[field] = value
		default:
			into, _ := object[field].(map[string]any)
			object[field] = mergePatch(into, nested)
		}
	}
	return object
}
