package main

import (
	"flag"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Usage:
//
//	go run .                                     # config.yaml, print tree + type summary
//	go run . -config examples/grouped/config.yaml
//	go run . -at /charlie/redis                  # merged values seen from that position
//	go run . -at /alpha/web-shop ../cluster/clusterName ../payment-api/application/port
//
// Any extra arguments are references resolved with lookup(), relative to -at.
func main() {
	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	position := flag.String("at", "", "point of view: a directory or file in the tree, e.g. /bravo/grafana")
	flag.Parse()

	config, err := loadConfig(*configPath)
	if err != nil {
		fail(err)
	}
	tree, err := loadTree(config)
	if err != nil {
		fail(err)
	}

	// With references on the command line, only resolve those.
	if references := flag.Args(); len(references) > 0 {
		for _, reference := range references {
			result, err := lookup(tree, path.Clean("/"+*position), reference)
			if err != nil {
				fmt.Printf("%-45s => ERROR %v\n", reference, err)
				continue
			}
			fmt.Printf("%-45s => %s\n", reference, describe(result))
		}
		return
	}

	// With a position, print the merged view as YAML: the values a consumer
	// (a Helm chart, a template, ...) at that position would get.
	if *position != "" {
		view, sources, err := buildView(tree, *position)
		if err != nil {
			fail(err)
		}
		fmt.Printf("# view of %s, built from (most important first):\n", path.Clean("/"+*position))
		for _, source := range sources {
			fmt.Printf("#   %-50s %s\n", source.Source, typeLabel(source))
		}
		output, err := yaml.Marshal(view)
		if err != nil {
			fail(err)
		}
		fmt.Print(string(output))
		return
	}

	fmt.Printf("== Types defined in %s ==\n", *configPath)
	for _, typeName := range slices.Sorted(maps.Keys(config.Types)) {
		fmt.Printf("  !%-12s matches %v\n", typeName, config.Types[typeName].Match)
	}

	fmt.Printf("\n== Tree of %s ==\n", config.DeploymentsDir)
	printTree(tree, 0)

	// Group every typed object by its type name. Type info is plain data
	// now (object.Type.Name), so this works for whatever types the user invents.
	fmt.Println("\n== Objects per type ==")
	objectsByType := map[string][]string{}
	walkLeaves(tree, "/", func(location string, value any) {
		if object := value.(*Object); object.Type != nil {
			objectsByType[object.Type.Name] = append(objectsByType[object.Type.Name], location)
		}
	})
	for _, typeName := range slices.Sorted(maps.Keys(objectsByType)) {
		fmt.Printf("  !%-12s %d: %s\n", typeName, len(objectsByType[typeName]), strings.Join(objectsByType[typeName], ", "))
	}
}

// describe formats a lookup result: typed objects show their type name.
func describe(value any) string {
	if object, isFile := value.(*Object); isFile {
		return fmt.Sprintf("%s %v", typeLabel(object), object.Data)
	}
	return fmt.Sprintf("%#v  (%T)", value, value)
}

// printTree prints directories with sorted keys, and each file with its type.
func printTree(node Directory, depth int) {
	for _, key := range slices.Sorted(maps.Keys(node)) {
		indent := strings.Repeat("  ", depth+1)
		switch value := node[key].(type) {
		case Directory:
			fmt.Printf("%s%s/\n", indent, key)
			printTree(value, depth+1)
		case *Object:
			fmt.Printf("%s%-14s %-13s %v\n", indent, key, typeLabel(value), value.Data)
		}
	}
}

// walkLeaves calls visit for every non-directory value, with its tree path.
func walkLeaves(node Directory, location string, visit func(string, any)) {
	for _, key := range slices.Sorted(maps.Keys(node)) {
		childLocation := path.Join(location, key)
		if child, isDirectory := node[key].(Directory); isDirectory {
			walkLeaves(child, childLocation, visit)
		} else {
			visit(childLocation, node[key])
		}
	}
}

// typeLabel renders an object's type as "!name", or "(untyped)".
func typeLabel(object *Object) string {
	if object.Type == nil {
		return "(untyped)"
	}
	return "!" + object.Type.Name
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
