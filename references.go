package main

import (
	"fmt"
	"strings"
)

// resolveAllReferences replaces every `!ref` in every file with the value it
// points to. It runs once, right after loading, so the rest of the program
// (views, lookups, validation) only ever sees plain values.
//
// A reference points to the raw content of another file, not to that file's
// merged view. `!ref ^cluster/domain` finds `domain` only if cluster.yaml
// itself sets it.
func resolveAllReferences(tree Directory) error {
	references := &resolver{tree: tree}

	// Resolve everything first, then swap the results in. That way every
	// lookup during resolution sees the same unchanged tree.
	resolvedData := map[*Object]any{}
	var firstError error
	walkLeaves(tree, "/", func(_ string, value any) {
		file := value.(*Object)
		if firstError != nil {
			return
		}
		resolved, err := references.resolveValue(file.Data)
		if err != nil {
			firstError = err
			return
		}
		resolvedData[file] = resolved
	})
	if firstError != nil {
		return firstError
	}
	for file, data := range resolvedData {
		file.Data = data
	}
	return nil
}

// resolveValue returns a copy of value with all references replaced.
func (r *resolver) resolveValue(value any) (any, error) {
	switch typed := value.(type) {
	case *Reference:
		return r.follow(typed)
	case *Object:
		// A reference to a whole file gives its content.
		return r.resolveValue(typed.Data)
	case Directory:
		return nil, fmt.Errorf("points to a directory, not to a file or value")
	case map[string]any:
		resolved := make(map[string]any, len(typed))
		for key, child := range typed {
			resolvedChild, err := r.resolveValue(child)
			if err != nil {
				return nil, err
			}
			resolved[key] = resolvedChild
		}
		return resolved, nil
	case []any:
		resolved := make([]any, len(typed))
		for index, child := range typed {
			resolvedChild, err := r.resolveValue(child)
			if err != nil {
				return nil, err
			}
			resolved[index] = resolvedChild
		}
		return resolved, nil
	default:
		return value, nil // strings, numbers, bools
	}
}

// follow resolves one reference. If following it leads back to a reference
// that is already being followed, that's a cycle and can never finish.
func (r *resolver) follow(reference *Reference) (any, error) {
	for index, active := range r.chain {
		if active == reference {
			var steps []string
			for _, step := range append(r.chain[index:], reference) {
				steps = append(steps, fmt.Sprintf("%s (!ref %s)", step.Source, step.Path))
			}
			return nil, fmt.Errorf("reference cycle:\n  %s", strings.Join(steps, "\n  -> "))
		}
	}
	r.chain = append(r.chain, reference)
	defer func() { r.chain = r.chain[:len(r.chain)-1] }()

	value, err := r.lookup(reference.From, reference.Path)
	if err != nil {
		return nil, fmt.Errorf("%s: !ref %s: %w", reference.Source, reference.Path, err)
	}
	return value, nil
}
