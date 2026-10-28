package main

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
)

// A reference path is made of segments separated by "/":
//
//	name      child: a directory, a file (without .yaml), or a key inside a file
//	0, 1, ... list index
//	..        parent. From a file, that's the file's directory, so in
//	          /shop/frontend.deployment, "../namespace" is the sibling file.
//	.         stay here
//	^type     the nearest file of that type, searching the current directory
//	          and then every directory above it. A file never finds itself,
//	          so from a cluster, "^cluster" is the next cluster further up.
//	/...      a leading slash starts at the root instead of the current position
//
// Examples, seen from /infra-1/tenant-a/apps/shop/application:
//
//	../image                       sibling file image.yaml, as a whole map
//	^cluster/clusterName           my cluster's name (tenant-a)
//	^cluster/^cluster/clusterName  my cluster's parent cluster's name (infra-1)
//
// "^" works without knowing the layout: it doesn't matter how many apps/ or
// other directories lie between an application and its cluster.

// lookup resolves a reference path, starting at the tree position `from`.
func lookup(tree Directory, from, ref string) (any, error) {
	return (&resolver{tree: tree}).lookup(from, ref)
}

// resolver navigates the tree. It carries the chain of references it is
// currently following, to catch cycles (a points to b, b points back to a).
type resolver struct {
	tree  Directory
	chain []*Reference
}

func (r *resolver) lookup(from, ref string) (any, error) {
	targetPath, err := r.resolvePath(from, ref)
	if err != nil {
		return nil, fmt.Errorf("resolving %q from %s: %w", ref, from, err)
	}
	value, err := r.walk(targetPath, true)
	if err != nil {
		return nil, fmt.Errorf("resolving %q from %s: %w", ref, from, err)
	}
	// The target may itself be, or contain, references.
	return r.resolveValue(value)
}

// resolvePath turns a reference into an absolute tree path, e.g.
// "^cluster/clusterName" from /a/b/apps/x -> "/a/b/cluster/clusterName".
func (r *resolver) resolvePath(from, ref string) (string, error) {
	current := from
	if strings.HasPrefix(ref, "/") {
		current = "/"
	}
	for _, segment := range strings.Split(ref, "/") {
		switch {
		case segment == "" || segment == ".":
			// nothing to do
		case segment == "..":
			current = path.Dir(current)
		case strings.HasPrefix(segment, "^"):
			found, err := r.findAbove(current, strings.TrimPrefix(segment, "^"))
			if err != nil {
				return "", err
			}
			current = found
		default:
			current = path.Join(current, segment)
		}
	}
	return current, nil
}

// findAbove implements "^type": find the nearest file of typeName, starting
// in the directory of `position` and moving up to the root.
func (r *resolver) findAbove(position, typeName string) (string, error) {
	if typeName == "" {
		return "", fmt.Errorf(`"^" needs a type name, like ^cluster`)
	}
	node, err := r.walk(position, false)
	if err != nil {
		return "", err
	}

	directoryPath := position
	var self *Object // a file must not find itself
	switch typed := node.(type) {
	case Directory:
	case *Object:
		directoryPath = path.Dir(position)
		self = typed
	default:
		return "", fmt.Errorf("^%s can only start at a directory or a file, but %s is a value inside a file", typeName, position)
	}

	for {
		directoryNode, _ := r.walk(directoryPath, false)
		directory := directoryNode.(Directory) // always a directory: we only ever move up from one
		var found []string
		for _, name := range slices.Sorted(maps.Keys(directory)) {
			file, isFile := directory[name].(*Object)
			if isFile && file != self && file.Type != nil && file.Type.Name == typeName {
				found = append(found, name)
			}
		}
		if len(found) == 1 {
			return path.Join(directoryPath, found[0]), nil
		}
		if len(found) > 1 {
			return "", fmt.Errorf("^%s is ambiguous in %s: %v", typeName, directoryPath, found)
		}
		if directoryPath == "/" {
			return "", fmt.Errorf("no !%s found above %s", typeName, position)
		}
		directoryPath = path.Dir(directoryPath)
	}
}

// walk goes from the root to an absolute tree path. With followReferences,
// references met along the way are resolved, so a path can continue through
// the value a reference points to.
func (r *resolver) walk(treePath string, followReferences bool) (any, error) {
	var current any = r.tree
	walked := ""
	for _, segment := range strings.Split(strings.TrimPrefix(treePath, "/"), "/") {
		if segment == "" {
			continue
		}
		if reference, isReference := current.(*Reference); isReference && followReferences {
			resolved, err := r.follow(reference)
			if err != nil {
				return nil, err
			}
			current = resolved
		}
		walked += "/" + segment
		next, err := step(current, segment)
		if err != nil {
			return nil, fmt.Errorf("at %s: %w", walked, err)
		}
		current = next
	}
	return current, nil
}

// step moves one level down: a map key, a list index, or into a file.
func step(current any, segment string) (any, error) {
	switch node := current.(type) {
	case *Object:
		// A file is transparent: its content is reachable directly.
		return step(node.Data, segment)
	case Directory:
		child, exists := node[segment]
		if !exists {
			return nil, fmt.Errorf("no such file or directory")
		}
		return child, nil
	case map[string]any:
		child, exists := node[segment]
		if !exists {
			return nil, fmt.Errorf("no such key")
		}
		return child, nil
	case []any:
		index, err := strconv.Atoi(segment)
		if err != nil || index < 0 || index >= len(node) {
			return nil, fmt.Errorf("list has %d items, %q is not a valid index", len(node), segment)
		}
		return node[index], nil
	default:
		return nil, fmt.Errorf("cannot descend into %T value %v", current, current)
	}
}
