# maia

**M**erged **A**ssembly of **I**nherited **A**ttributes: a consumer-agnostic
loader for hierarchical, typed YAML data.

MAIA reads a tree of YAML files, gives every file a user-defined type,
validates it, resolves references between files, and assembles the values
visible from any position in the tree into one map. What consumes that map
is not its concern: Helm charts, kustomize, an Ansible inventory, or anything
else that eats YAML. It is meant as the value-loading core for
[ATLAS](https://github.com/max06/atlas), and as an alternative to tools like
Hiera or reclass.

> **Status:** early prototype. Everything here is a demo used to explore the
> design. Expect breaking changes. No license has been chosen yet.

## Quick start

Requires Go 1.27.

```bash
go run .                                          # print the tree and types for ./config.yaml
go run . -at /charlie/redis                       # merged values seen from a position
go run . -config examples/nested/config.yaml \
  -at /infra-1/tenant-a/apps/shop/application \
  ^cluster/clusterName ^cluster/^cluster/clusterName   # resolve references
```

## Concepts (as implemented in the demo)

### Types live in config, not in code

```yaml
deploymentsDir: deployments
types:
  cluster:
    match: ["cluster.yaml"]   # optional, default "<name>.yaml"
    scope: file               # optional, "directory" (default) or "file"
    schema:                   # optional JSON Schema 2020-12, written as YAML
      type: object
      additionalProperties: false
      required: [clusterName]
      properties:
        clusterName: { type: string }
```

- A file's type comes from its filename (`match` globs) or an explicit
  `--- !typename` tag at the top of the file. Files matching no type are
  loaded untyped.
- Each type's schema decides how strict validation is: fully closed with
  `additionalProperties: false`, or a few required keys plus free-form maps.
- `config.yaml` itself is decoded strictly, and schemas are checked against
  the JSON Schema metaschema at startup.

### The tree

Directories become `Directory` nodes, files become `*Object` nodes (type,
source path, data), keyed by filename without extension.

### Views: a point of view in the tree

`-at <position>` merges everything visible from a directory or file:

1. The position itself (if it is a file).
2. Every file in the position's directory and each directory above it, up to
   the root, whose type has `scope: directory`. Sibling directories are never
   visited.

Values closer to the position win. Maps merge key by key, lists and scalars
are replaced as a whole. Two files in the *same* directory are equally
specific, so a disagreement between them is an error, never a silent pick.

`scope: file` keeps a type's values out of everything except a view of that
exact file. Use it for peers sharing a directory, and to prevent implicit
leaking (for example a cluster's `domain` ending up in a chart's values).

### References

```yaml
values:
  clusterName: !ref ^cluster/clusterName            # nearest cluster above
  hostCluster: !ref ^cluster/^cluster/clusterName   # that cluster's parent
  image: !ref ../image                              # sibling file image.yaml
```

- Paths are relative to the file they are written in. From a file, `..` is
  the file's directory. A leading `/` starts at the root.
- `^type` finds the nearest file of that type in the current directory or
  above. A file never finds itself, so a cluster's `^cluster` is its parent.
- References are resolved after loading and before validation, so schemas
  check the referenced value. Cycles are detected and reported.
- A reference points at a file's raw content, not at its merged view.

### Examples

| Directory | Layout |
|---|---|
| `config.yaml` + `deployments/` | cluster → application, with layered `defaults.yaml` |
| `examples/grouped/` | group → cluster → application |
| `examples/namespaced/` | single cluster, namespaces, `*.deployment.yaml` files with `scope: file` |
| `examples/nested/` | host cluster containing a virtual cluster, explicit wiring with `!ref`, strict `image.yaml` |

## Design direction

Agreed next steps, in order. Each step updates the spec and adds conformance
tests.

1. **JSON Reference instead of `!ref`.** `$ref: ../image.yaml#/tag` (file
   path plus JSON Pointer), as in JSON Schema and OpenAPI. Keys next to `$ref`
   override the referenced value. The nearest-ancestor lookup needs a
   URI-compatible form, for example `$ref: "ancestor:cluster#/clusterName"`.
2. **Instances and multiple roots.** A file can expand into several positions
   (for example one per entry in a deployment's app list), so a reusable
   template only ever sees one instance's values. Templates live outside the
   tree, reachable through named roots configured in `config.yaml`.
3. **Merge strategies in the schema.** Per-key annotations such as
   `x-merge: none | replace | deep | map` (with `x-merge-key`), modelled on
   Kubernetes' `x-kubernetes-list-type` and `x-kubernetes-map-type`.

Later, inside ATLAS or as extension modules:

- **Loading steps per type:** for example `load: [sops, yaml]` for
  `*.sops.yaml` (via the SOPS Go library, no external binaries), or templated
  value files. Values from secret sources are marked as secret, so outputs can
  redact them.
- **Output adapters:** Helm values, kustomize inputs, Ansible inventory JSON.

Guiding principles:

- **Explicit over implicit.** The main editors of MAIA-based repositories are
  expected to be AI agents. Explicit definitions, strict validation, clear
  errors and a per-value provenance (`explain`) beat conventions that have to
  be known.
- **Standards where they exist:** JSON Schema, JSON Pointer, JSON Reference.

## Prior art

Each of these covers part of the problem: Hiera (per-key merge strategies,
explicit hierarchy), reclass / Kapitan (class-based deep merge and `${...}`
references, but awkward for multiple instances of one template), CUE
(per-directory views and schemas, but no overrides), Nickel (merge
priorities), Pkl and Jsonnet (override chains), Terragrunt
(`find_in_parent_folders`), and XPath (`ancestor::` is the closest match to
`^type`).
