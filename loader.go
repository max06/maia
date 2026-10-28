package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Directory is a directory in the tree. It is its own named type (instead of
// a plain map[string]any) so a directory can never be confused with the
// content of an untyped YAML file, which also decodes to map[string]any.
type Directory map[string]any

// Object is one file in the tree: its content plus the user-defined type it
// belongs to. This is how type info survives at runtime now that types aren't
// Go structs anymore. Untyped files are Objects too, with Type == nil, so
// every value in the tree can be traced back to its file.
type Object struct {
	Type   *TypeDefinition // nil for untyped files
	Source string          // file path, for error messages
	Data   any             // the file's content: map[string]any, []any, string, ...
}

// Reference is a `!ref some/path` value, waiting to be resolved (see
// references.go). It remembers where it was written, because relative paths
// start from there.
type Reference struct {
	Path   string // the path as written, e.g. "^cluster/clusterName"
	From   string // tree position of the file it's in, e.g. "/alpha/web-shop/application"
	Source string // file path and line, for error messages
}

// loadTree builds the whole tree in three phases:
//
//  1. Read every YAML file into one nested map that mirrors the directories:
//     tree["alpha"]["cluster"]                 -> *Object{Type: cluster, ...}
//     tree["alpha"]["web-shop"]["application"] -> *Object{Type: application, ...}
//     Directories become Directory. Each file is keyed by its name without
//     extension, and holds an *Object. `!ref` values become *Reference.
//  2. Replace every *Reference with the value it points to.
//  3. Validate every typed file against its schema.
//
// Validation comes last on purpose: a schema should check the value a
// reference points to, not the reference itself.
func loadTree(config *Config) (Directory, error) {
	tree := Directory{}

	err := filepath.WalkDir(config.DeploymentsDir, func(filePath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		extension := filepath.Ext(filePath)
		if entry.IsDir() || (extension != ".yaml" && extension != ".yml") {
			return nil
		}

		// Walk/create the nested maps for each directory level.
		relativePath, _ := filepath.Rel(config.DeploymentsDir, filePath)
		pathSegments := strings.Split(filepath.ToSlash(relativePath), "/")
		directoryMap := tree
		for _, directoryName := range pathSegments[:len(pathSegments)-1] {
			child, exists := directoryMap[directoryName].(Directory)
			if !exists {
				if _, taken := directoryMap[directoryName]; taken {
					return fmt.Errorf("%s: directory %q collides with a file of the same name", filePath, directoryName)
				}
				child = Directory{}
				directoryMap[directoryName] = child
			}
			directoryMap = child
		}
		fileKey := strings.TrimSuffix(pathSegments[len(pathSegments)-1], extension)
		if _, taken := directoryMap[fileKey]; taken {
			// e.g. "cluster.yaml" next to a "cluster/" directory or "cluster.yml"
			return fmt.Errorf("%s: name %q is already taken in this directory", filePath, fileKey)
		}

		// The file's own position in the tree, e.g. "/alpha/web-shop/application".
		// References inside the file are resolved relative to it.
		treePosition := "/" + path.Join(append(pathSegments[:len(pathSegments)-1], fileKey)...)
		object, err := loadFile(filePath, treePosition, config)
		if err != nil {
			return err
		}
		directoryMap[fileKey] = object
		return nil
	})
	if err != nil {
		return nil, err
	}

	if err := resolveAllReferences(tree); err != nil {
		return nil, err
	}
	if err := validateTree(tree); err != nil {
		return nil, err
	}
	return tree, nil
}

// loadFile reads a file and decides its type. It does not validate. That
// happens in validateTree, after references are resolved.
//
// The type comes from one of two places:
//  1. An explicit YAML tag at the top of the file, e.g. `--- !application`.
//  2. Otherwise the type whose `match` patterns fit the filename.
func loadFile(filePath, treePosition string, config *Config) (*Object, error) {
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("%s: %w", filePath, err)
	}
	if len(document.Content) == 0 {
		return &Object{Source: filePath}, nil // empty file: untyped, no data
	}
	rootNode := document.Content[0]

	var typeDefinition *TypeDefinition
	if isCustomTag(rootNode.Tag) {
		typeName := strings.TrimPrefix(rootNode.Tag, "!")
		typeDefinition = config.Types[typeName]
		if typeDefinition == nil {
			return nil, fmt.Errorf("%s: tag !%s, but config.yaml defines no type %q", filePath, typeName, typeName)
		}
		rootNode.Tag = "" // consumed: it named the type, it's not part of the data
	} else {
		typeDefinition, err = config.typeForFile(filepath.Base(filePath))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filePath, err)
		}
	}

	data, err := nodeToValue(rootNode, filePath, treePosition)
	if err != nil {
		return nil, err
	}
	return &Object{Type: typeDefinition, Source: filePath, Data: data}, nil
}

// isCustomTag reports whether a YAML tag is user-written like "!ref", as
// opposed to YAML's built-in tags like "!!str" or "!!map".
func isCustomTag(tag string) bool {
	return strings.HasPrefix(tag, "!") && !strings.HasPrefix(tag, "!!")
}

// nodeToValue converts a parsed YAML node into plain Go values, the same way
// the YAML library's own decoding would, with one addition: a scalar tagged
// `!ref` becomes a *Reference. The library can't do that for us when decoding
// into `any`, so this walks the nodes by hand.
func nodeToValue(node *yaml.Node, filePath, treePosition string) (any, error) {
	location := fmt.Sprintf("%s:%d", filePath, node.Line)
	if isCustomTag(node.Tag) && node.Tag != "!ref" {
		return nil, fmt.Errorf("%s: unknown tag %s (only !ref is supported inside a file)", location, node.Tag)
	}

	switch node.Kind {
	case yaml.MappingNode:
		result := make(map[string]any, len(node.Content)/2)
		// Content holds keys and values alternating: key, value, key, value, ...
		for index := 0; index < len(node.Content); index += 2 {
			keyNode, valueNode := node.Content[index], node.Content[index+1]
			if keyNode.Tag == "!!merge" {
				return nil, fmt.Errorf("%s: YAML merge keys (<<) are not supported in this demo", location)
			}
			if keyNode.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("%s: map keys must be plain strings", location)
			}
			value, err := nodeToValue(valueNode, filePath, treePosition)
			if err != nil {
				return nil, err
			}
			result[keyNode.Value] = value
		}
		return result, nil

	case yaml.SequenceNode:
		result := make([]any, 0, len(node.Content))
		for _, itemNode := range node.Content {
			item, err := nodeToValue(itemNode, filePath, treePosition)
			if err != nil {
				return nil, err
			}
			result = append(result, item)
		}
		return result, nil

	case yaml.AliasNode: // *anchor: use the anchored node's value
		return nodeToValue(node.Alias, filePath, treePosition)

	case yaml.ScalarNode:
		if node.Tag == "!ref" {
			return &Reference{Path: node.Value, From: treePosition, Source: location}, nil
		}
		// Let the YAML library decide what the scalar is: 3 -> int,
		// true -> bool, "3" -> string, ...
		var value any
		if err := node.Decode(&value); err != nil {
			return nil, fmt.Errorf("%s: %w", location, err)
		}
		return value, nil
	}
	return nil, fmt.Errorf("%s: unexpected YAML node kind %v", location, node.Kind)
}

// validateTree checks every typed file against its type's JSON Schema.
// Whether that's strict or minimal is entirely up to the schema in config.yaml.
func validateTree(directory Directory) error {
	for _, key := range slices.Sorted(maps.Keys(directory)) {
		switch node := directory[key].(type) {
		case Directory:
			if err := validateTree(node); err != nil {
				return err
			}
		case *Object:
			if node.Type == nil {
				continue // untyped: nothing to validate against
			}
			jsonData, err := toJSONCompatible(node.Data)
			if err != nil {
				return fmt.Errorf("%s: %w", node.Source, err)
			}
			if err := node.Type.compiledSchema.Validate(jsonData); err != nil {
				return fmt.Errorf("%s (!%s): %w", node.Source, node.Type.Name, err)
			}
		}
	}
	return nil
}

// toJSONCompatible converts YAML-decoded data into exactly the shapes a JSON
// decoder would produce, which is what the schema validator expects. A JSON
// round-trip is the simplest way: it turns ints into json.Number and rejects
// things JSON can't represent, like non-string map keys.
func toJSONCompatible(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("not representable as JSON: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber() // keep 3 as "3", not 3.0, so "type: integer" works
	var result any
	err = decoder.Decode(&result)
	return result, err
}
