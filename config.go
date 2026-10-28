package main

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

// Config mirrors config.yaml.
type Config struct {
	// DeploymentsDir is resolved relative to the config file's directory.
	DeploymentsDir string `yaml:"deploymentsDir"`

	// Types are defined entirely by the user: name -> definition.
	Types map[string]*TypeDefinition `yaml:"types"`
}

// TypeDefinition is one user-defined type from config.yaml.
//
// Since types now come from a file, Go can't turn them into structs (structs
// only exist at compile time). Instead a type is data: a name, a rule for
// which files belong to it, and a JSON Schema describing valid content.
type TypeDefinition struct {
	// Name is filled in from the map key in config.yaml, not written by the user.
	Name string `yaml:"-"`

	// Match lists glob patterns matched against a file's base name.
	// Defaults to ["<name>.yaml"]. Example: ["*.deployment.yaml"].
	Match []string `yaml:"match"`

	// Scope controls who sees this type's values in a view (see view.go):
	//   "directory" (default): everything in the file's directory and below.
	//                          Use it for cluster.yaml, defaults.yaml, ...
	//   "file":                only a view of this exact file. Use it for
	//                          peers sharing a directory, like several
	//                          *.deployment.yaml files that must not leak
	//                          values into each other.
	Scope string `yaml:"scope"`

	// Schema is a JSON Schema, written as YAML. Anything JSON Schema 2020-12
	// supports works here: required keys, patterns, enums, nested objects,
	// $defs and $ref, ...
	Schema any `yaml:"schema"`

	// compiledSchema is the ready-to-use validator built from Schema.
	compiledSchema *jsonschema.Schema
}

func loadConfig(configPath string) (*Config, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	// Strict decoding: a typo in config.yaml (e.g. "schemas:") is an error
	// instead of being silently ignored.
	var config Config
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("%s: %w", configPath, err)
	}
	config.DeploymentsDir = filepath.Join(filepath.Dir(configPath), config.DeploymentsDir)

	// Fill in defaults, then compile every schema once up front. The compiler
	// also checks each schema against the JSON Schema spec, so a broken
	// schema fails here, before any deployment file is read.
	compiler := jsonschema.NewCompiler()
	for typeName, typeDefinition := range config.Types {
		if typeDefinition == nil {
			typeDefinition = &TypeDefinition{}
			config.Types[typeName] = typeDefinition
		}
		typeDefinition.Name = typeName
		if len(typeDefinition.Match) == 0 {
			typeDefinition.Match = []string{typeName + ".yaml"}
		}
		switch typeDefinition.Scope {
		case "":
			typeDefinition.Scope = "directory"
		case "directory", "file":
		default:
			return nil, fmt.Errorf("%s: type %q: scope must be \"directory\" or \"file\", got %q", configPath, typeName, typeDefinition.Scope)
		}
		for _, pattern := range typeDefinition.Match {
			if _, err := path.Match(pattern, ""); err != nil {
				return nil, fmt.Errorf("%s: type %q: bad match pattern %q: %w", configPath, typeName, pattern, err)
			}
		}

		// No schema given means "anything goes". `true` is the JSON Schema
		// way of saying that.
		if typeDefinition.Schema == nil {
			typeDefinition.Schema = true
		}
		schemaDocument, err := toJSONCompatible(typeDefinition.Schema)
		if err != nil {
			return nil, fmt.Errorf("%s: type %q: %w", configPath, typeName, err)
		}
		// The URL is only an internal name for the schema. It shows up in
		// error messages, so it reads like where the schema came from.
		schemaURL := "file:///" + filepath.ToSlash(configPath) + "/types/" + typeName
		if err := compiler.AddResource(schemaURL, schemaDocument); err != nil {
			return nil, fmt.Errorf("%s: type %q: %w", configPath, typeName, err)
		}
		typeDefinition.compiledSchema, err = compiler.Compile(schemaURL)
		if err != nil {
			return nil, fmt.Errorf("%s: type %q: invalid schema: %w", configPath, typeName, err)
		}
	}
	return &config, nil
}

// typeForFile returns the type whose Match patterns fit the file's base name,
// or nil for untyped files. If two types claim the same file, that's an error.
// Silently picking one would depend on map order, which Go randomizes.
func (config *Config) typeForFile(fileName string) (*TypeDefinition, error) {
	var matches []string
	for typeName, typeDefinition := range config.Types {
		for _, pattern := range typeDefinition.Match {
			if matched, _ := path.Match(pattern, fileName); matched {
				matches = append(matches, typeName)
				break
			}
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return config.Types[matches[0]], nil
	default:
		slices.Sort(matches)
		return nil, fmt.Errorf("file %q matches several types: %v", fileName, matches)
	}
}
