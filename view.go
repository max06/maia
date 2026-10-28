package main

import (
	"fmt"
	"maps"
	"path"
	"reflect"
	"slices"
	"strings"
)

// buildView merges all values visible from one position in the tree into a
// single map. The position is a directory or a file, e.g. "/bravo/grafana".
//
// Which files are visible:
//   - the position itself, if it is a file
//   - every file in the position's directory and in each directory above it,
//     up to the root, as long as its type has scope "directory"
//     (untyped files always count as scope "directory")
//
// Sibling directories are never visited. /bravo/grafana sees /bravo/cluster,
// but never /bravo/loki/application.
//
// How values merge: closer to the position wins. The walk starts at the
// position and moves up, and each level only fills in keys that are still
// missing, so nothing set by a more specific file is ever overwritten. Maps
// merge key by key at any depth. Lists and plain values are replaced as a
// whole.
//
// Two files on the same level (the same directory) are equally specific.
// If they disagree on a value, there is no fair winner, so that's an error.
//
// sources lists the contributing files, most important first.
func buildView(tree Directory, position string) (view map[string]any, sources []*Object, err error) {
	position = path.Clean("/" + position)

	// Walk down from the root to the position, remembering every directory
	// on the way. The last one is the position itself, or the directory that
	// contains it when the position is a file.
	directoriesOnPath := []Directory{tree}
	var positionFile *Object
	var current any = tree
	for _, segment := range strings.Split(strings.TrimPrefix(position, "/"), "/") {
		if segment == "" {
			continue
		}
		directory, isDirectory := current.(Directory)
		if !isDirectory {
			return nil, nil, fmt.Errorf("view %q: position must be a directory or a file, not something inside a file", position)
		}
		child, exists := directory[segment]
		if !exists {
			return nil, nil, fmt.Errorf("view %q: %q not found", position, segment)
		}
		switch typed := child.(type) {
		case Directory:
			directoriesOnPath = append(directoriesOnPath, typed)
		case *Object:
			positionFile = typed
		}
		current = child
	}

	// Collect the levels, most specific first. Each level is a list of files
	// that are equally important.
	var levels [][]*Object
	if positionFile != nil {
		levels = append(levels, []*Object{positionFile})
	}
	for _, directory := range slices.Backward(directoriesOnPath) {
		var level []*Object
		for _, name := range slices.Sorted(maps.Keys(directory)) {
			file, isFile := directory[name].(*Object)
			if !isFile || file == positionFile {
				continue // subdirectory, or the position file (already its own level)
			}
			if file.Type != nil && file.Type.Scope == "file" {
				continue // peer of the position, its values are private
			}
			level = append(level, file)
		}
		if len(level) > 0 {
			levels = append(levels, level)
		}
	}

	view = map[string]any{}
	for _, level := range levels {
		levelValues, err := mergeLevel(level)
		if err != nil {
			return nil, nil, fmt.Errorf("view %q: %w", position, err)
		}
		fillMissing(view, levelValues)
		sources = append(sources, level...)
	}
	return view, sources, nil
}

// mergeLevel combines the files of one level. They are equally important, so
// any value they disagree on is reported instead of silently picking one.
func mergeLevel(files []*Object) (map[string]any, error) {
	levelValues := map[string]any{}
	owners := map[string]string{} // key path -> file that set it, for error messages
	for _, file := range files {
		if file.Data == nil {
			continue // empty file
		}
		fileValues, isMap := file.Data.(map[string]any)
		if !isMap {
			return nil, fmt.Errorf("%s: a file must contain a map to be merged into a view, got %T", file.Source, file.Data)
		}
		if err := mergeStrict(levelValues, fileValues, "", file.Source, owners); err != nil {
			return nil, err
		}
	}
	return levelValues, nil
}

// mergeStrict copies src into dst. Nested maps merge. Any other value already
// present in dst must be equal, or that's a conflict.
func mergeStrict(dst, src map[string]any, keyPath, sourceFile string, owners map[string]string) error {
	for _, key := range slices.Sorted(maps.Keys(src)) {
		childPath := keyPath + "/" + key
		existing, exists := dst[key]
		if !exists {
			dst[key] = deepCopy(src[key])
			owners[childPath] = sourceFile
			continue
		}
		existingMap, existingIsMap := existing.(map[string]any)
		newMap, newIsMap := src[key].(map[string]any)
		if existingIsMap && newIsMap {
			if err := mergeStrict(existingMap, newMap, childPath, sourceFile, owners); err != nil {
				return err
			}
			continue
		}
		if !reflect.DeepEqual(existing, src[key]) {
			return fmt.Errorf("conflict at %s: %s says %v, %s says %v (same level, no winner)",
				childPath, ownerOf(childPath, owners), existing, sourceFile, src[key])
		}
	}
	return nil
}

// ownerOf finds which file set a key path. If a whole map was copied in one
// go, only the map's own path is recorded, so it also checks the parents.
func ownerOf(keyPath string, owners map[string]string) string {
	for current := keyPath; current != "/" && current != "."; current = path.Dir(current) {
		if owner, found := owners[current]; found {
			return owner
		}
	}
	return "?"
}

// fillMissing copies keys from src into dst that dst doesn't have yet.
// Nested maps are merged the same way. Everything else in dst stays.
func fillMissing(dst, src map[string]any) {
	for key, srcValue := range src {
		existing, exists := dst[key]
		if !exists {
			dst[key] = deepCopy(srcValue)
			continue
		}
		existingMap, existingIsMap := existing.(map[string]any)
		srcMap, srcIsMap := srcValue.(map[string]any)
		if existingIsMap && srcIsMap {
			fillMissing(existingMap, srcMap)
		}
	}
}

// deepCopy copies maps and lists, so a view never shares memory with the
// tree. Changing a view must not change the files' data.
func deepCopy(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		copied := make(map[string]any, len(typed))
		for key, child := range typed {
			copied[key] = deepCopy(child)
		}
		return copied
	case []any:
		copied := make([]any, len(typed))
		for index, child := range typed {
			copied[index] = deepCopy(child)
		}
		return copied
	default:
		return value // strings, numbers, bools: copied by value anyway
	}
}
