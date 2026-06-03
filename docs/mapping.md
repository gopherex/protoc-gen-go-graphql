# Proto → GraphQL Mapping Reference

This document describes how `protoc-gen-go-graphql` maps proto constructs to
GraphQL, the two-phase generation model, the command sequence, and the output
layout.

---

## Mapping table

| Proto construct | GraphQL output | Notes |
|---|---|---|
| file | `schema.graphql` + `gqlgen.yml` | one set of artifacts per processed file |
| message (response) | output `type` | bound directly to `*pb.Msg` via gqlgen autobind |
| message (request) | `input` type | same `*pb.Msg` Go type reused; request and response bind to the same struct |
| empty message (output / nested) | `type X { ok: Boolean! }` / `input XInput { _empty: Boolean }` | GraphQL forbids fieldless types, so a placeholder field is emitted (`@goField(forceResolver:true)`) with a no-op resolver; empty top-level requests instead drop the input arg entirely |
| message-only / all-skipped package | _no `gqlapi` emitted_ | a file with no enabled service method has no Query/Mutation/Subscription root; it is skipped (still importable/bindable by API packages that reference it) |
| enum | `enum` (member names = proto value names) | e.g. `FICTION`, `GENRE_UNSPECIFIED`; marshal/unmarshal adapters in `pbgql` |
| unary rpc, `NO_SIDE_EFFECTS` | `Query` field | default from builtin `idempotency_level` |
| unary rpc, `IDEMPOTENT` or `IDEMPOTENCY_UNKNOWN` | `Mutation` field | derived from builtin `idempotency_level` |
| server-streaming rpc | `Subscription` field | resolver returns `<-chan *pb.T`; runtime pumps the gRPC stream |
| **bidi-streaming rpc** | **hard generation error** | not supported; see §Supported/Unsupported |
| **client-streaming rpc** | **hard generation error** | not supported; see §Supported/Unsupported |
| oneof (output field) | GraphQL `union` | wrapper types in `pbgql`; see `docs/oneof.md` |
| oneof (input field) | `@oneOf` input + intermediate struct in `pbgql` | see `docs/oneof.md` |
| `map<K,V>` | `JSON` scalar + `@goField(forceResolver:true)` | protojson-aligned; map fields on input objects are omitted (v1 limitation) |
| `int64` / `sint64` / `sfixed64` | `Int64` scalar (String) | protojson encodes 64-bit as decimal strings; GraphQL `Int` is 32-bit |
| `uint64` / `fixed64` | `Uint64` scalar (String) | same reason |
| `bytes` | `Bytes` scalar (base64 String) | standard base64 encoding, protojson-aligned |
| `google.protobuf.Timestamp` | `Timestamp` scalar | RFC 3339 with nanoseconds, e.g. `"2024-01-15T10:30:00.123456789Z"` |
| `google.protobuf.Duration` | `Duration` scalar | canonical proto3-JSON form, e.g. `"1.5s"` |
| `google.protobuf.Struct` / `Value` / `Any` / `ListValue` | `JSON` scalar | pass-through; no structural typing |
| gRPC `status` error | GraphQL error | `extensions.code` = SCREAMING_SNAKE_CASE code name (e.g. `NOT_FOUND`); message = status message; `extensions.details` = structured (see below) |

Operation type (`Query` vs `Mutation`) is derived from the builtin
`google.protobuf.MethodOptions.idempotency_level`. No custom proto option is
needed — the default rule fully covers all practical cases. Methods with
`idempotency_level = IDEMPOTENT` additionally carry the `@idempotent` schema
directive on their Mutation field (introspectable metadata for client retry/dedup).

### gRPC error mapping (`graphqlpb.GraphQLError`)

- **code** → `extensions.code` (SCREAMING_SNAKE_CASE, e.g. `NOT_FOUND`).
- **message** → the GraphQL error message.
- **details** → `extensions.details`: each `status` detail (`google.rpc.ErrorInfo`,
  `BadRequest`, `QuotaFailure`, …) is protojson-marshaled into a structured object
  tagged with `"@type"` (e.g. `"google.rpc.ErrorInfo"`). Error metadata travels
  here, in `ErrorInfo.metadata` — that is the gRPC-standard place for it.
- **transport metadata / trailers** (`grpc.SetHeader` / `grpc.SetTrailer`) are
  **NOT** surfaced. The generated resolvers delegate to the gRPC server
  **in-process** (no transport), so headers/trailers never travel. Put any
  error-side key/values in `google.rpc.ErrorInfo.metadata` (a status detail),
  which IS surfaced. Success-path trailers would require a client-based (network)
  delegation, which is out of scope for the in-process model.

### Custom scalars are protojson-aligned

All custom scalar implementations in `graphqlpb/scalars.go` produce output
byte-compatible with protobuf-es `toJson`/`fromJson`. A TypeScript client using
protobuf-es can parse GraphQL field values directly without conversion for 64-bit
integers, enums, Timestamp, Duration, bytes, and `Struct`/`Any`/`Value`.

---

## Two-phase generation

### Why two phases

gqlgen's binder loads the bound model package (`pb`) via `go/packages` **at
generate time** — it type-checks `pb.Book`, inspects field types, resolves oneof
interfaces, and emits field-access code (`obj.Name`, getter calls, wrapper
dispatch). It cannot run until the `pb` package exists on disk.

In a single `protoc` run every plugin's output is written **after all plugins
finish**. That means `*.pb.go` is not on disk while our plugin executes. gqlgen
must therefore run in a separate step after the `protoc` pass completes. This is
the standard gqlgen workflow (models first, gqlgen second) — not a workaround.

### Phase A — the protoc plugin

Runs inside the `protoc` invocation (alongside `--go_out` and `--go-grpc_out`).
Emits:

| Artifact | Description |
|---|---|
| `gqlapi/schema.graphql` | GraphQL SDL: types, inputs, enums, unions, Query/Mutation/Subscription, scalar declarations, directives |
| `gqlapi/gqlgen.yml` | gqlgen configuration: `autobind` to the pb import path, `models:` for custom scalars and oneof adapters, exec output paths |
| `gqlapi/resolver.go` | Delegating resolver: implements `exec.ResolverRoot`; delegates each operation directly to the gRPC server implementation |
| `gqlapi/pbgql/` | Enum marshal/unmarshal adapters, oneof union interfaces/wrappers, oneof input structs + `ToPb*` shims |
| `gqlapi/generate.go` | Contains `//go:generate go run github.com/gopherex/protoc-gen-go-graphql/cmd/gqlgenrun --config gqlgen.yml` |

### Phase B — `go generate`

Triggered by `go generate ./...` (or the `go:generate` directive in
`gqlapi/generate.go`). Runs `cmd/gqlgenrun`, which calls
`config.LoadConfig("gqlgen.yml")` then `api.Generate(cfg)` in-process. gqlgen
reads the now-on-disk pb package, autobinds types, and emits:

| Artifact | Description |
|---|---|
| `gqlapi/exec/exec.go` | gqlgen execution engine (`NewExecutableSchema`, resolver interfaces) |
| `gqlapi/models_gen.go` | Any model types gqlgen had to generate (typically none; most are covered by autobind) |

`cmd/gqlgenrun` must live in its own subdirectory (not inside `gqlapi/`). gqlgen's
package loader reads every file in the resolver directory; a `package main` there
conflicts with the resolver package name even with `//go:build ignore`.

---

## Plugin flags

Pass flags to the plugin via `--go-graphql_opt=<flag>=<value>` in the protoc
invocation (multiple flags use multiple `--go-graphql_opt=` arguments).

| Flag | Default | Description |
|---|---|---|
| `paths` | _(empty)_ | Path mode; set to `source_relative` for source-relative output paths |
| `out_dir` | `gqlapi` | Subpackage directory name and Go package name for generated GraphQL code. Override to rename the `gqlapi/` subdirectory and package. |
| `runner` | `github.com/gopherex/protoc-gen-go-graphql/cmd/gqlgenrun` | Import path of the `go:generate` runner binary. Override for forks or vendored copies. |
| `single_pass` | `false` | Run gqlgen inside the plugin (no separate `go generate` step); emits `exec/exec.go` + `models_gen.go` directly. Requires `protoc-gen-go` on `PATH`. |

`single_pass` runs gqlgen against a throwaway module assembled from the user's
`go.mod`. It regenerates the pb for the proto files in the current protoc request
and **copies any other in-module pb packages those messages import** (e.g. shared
`models`/`deployment` packages) from the user's source tree, so they must already
be generated on disk. Packages whose every service is skipped (or that define only
messages) are skipped entirely — no `go list`/gqlgen runs for them.

Example — rename the output package to `graphql`:

```sh
--go-graphql_opt=paths=source_relative \
--go-graphql_opt=out_dir=graphql
```

---

## Command sequence

```sh
# Step 1: build the three plugins (once, or when source changes)
make build
# Equivalent:
#   go build -o bin/protoc-gen-go-graphql ./
#   go build -o bin/protoc-gen-go  google.golang.org/protobuf/cmd/protoc-gen-go
#   go build -o bin/protoc-gen-go-grpc google.golang.org/grpc/cmd/protoc-gen-go-grpc

# Step 2: Phase A — run protoc with all three plugins
protoc -I example/ -I . -I /usr/include \
    --plugin=protoc-gen-go=bin/protoc-gen-go \
    --plugin=protoc-gen-go-grpc=bin/protoc-gen-go-grpc \
    --plugin=protoc-gen-go-graphql=bin/protoc-gen-go-graphql \
    --go_out=example/gen      --go_opt=paths=source_relative \
    --go-grpc_out=example/gen --go-grpc_opt=paths=source_relative \
    --go-graphql_out=example/gen --go-graphql_opt=paths=source_relative \
    example/golden.proto

# Step 3: Phase B — run gqlgen (loads pb, emits exec engine)
cd example/gen && go generate ./...
```

The `WKT_INC` variable (`/usr/include` by default) points to the directory
containing the well-known-type `.proto` files (`google/protobuf/*.proto`). Override
it if your protoc installation places them elsewhere:

```sh
make gen-test WKT_INC=/usr/local/include
```

The canonical example is the `gen-test` Makefile target, which runs both phases.

---

## Output layout

After both phases, the output under the `--go-graphql_out` directory looks like:

```
gen/
  golden.pb.go          # protoc-gen-go: proto message types
  golden_grpc.pb.go     # protoc-gen-go-grpc: gRPC server/client stubs
  gqlapi/               # protoc-gen-go-graphql phase-A output
    schema.graphql
    gqlgen.yml
    generate.go         # //go:generate directive
    resolver.go         # delegating resolver (implements exec.ResolverRoot)
    pbgql/
      genre.go          # enum alias + Marshal/Unmarshal funcs
      searchrequest_oneof.go   # @oneOf input struct + ToPb shim
      searchresponse_oneof.go  # union interface + wrapper structs
    exec/               # gqlgen phase-B output
      exec.go           # execution engine (NewExecutableSchema + resolver interfaces)
    models_gen.go       # gqlgen phase-B: any additional generated models
```

### Why `gqlapi/` is a separate package from `pb`

The `exec` package (generated by gqlgen) imports the pbgql wrapper types and the
autobiound pb types. The `gqlapi` package imports `exec` to implement
`ResolverRoot`. If the resolver code lived in the same package as `pb`, the import
graph would form a cycle. Placing GraphQL artifacts in `gqlapi/` (a sibling to the
pb package) keeps the graph acyclic:

```
exec  → pbgql
exec  → pb
gqlapi → exec
gqlapi → pbgql
gqlapi → pb
```

---

## Supported / unsupported

| Feature | Status |
|---|---|
| Unary query (NO_SIDE_EFFECTS) | Supported |
| Unary mutation (IDEMPOTENT / IDEMPOTENCY_UNKNOWN) | Supported |
| Server-streaming subscription | Supported |
| Output oneof → union | Supported; see `docs/oneof.md` |
| Input oneof → @oneOf | Supported; see `docs/oneof.md` |
| `map<K,V>` on output | Supported (JSON scalar + field resolver) |
| `map<K,V>` on input | Not supported in v1 (field omitted from input type) |
| bidi-streaming rpc | Hard generation error |
| client-streaming rpc | Hard generation error |
| `FieldOptions.scalar` (custom scalar binding) | Intentionally unsupported (fail-fast) — see below |
| TypeScript code generation | Out of scope; use standard graphql-code-generator |

### `FieldOptions.scalar` is a documented non-goal

The `graphqlopt.FieldOptions.scalar` option (which would bind a proto field to a
user-named custom GraphQL scalar) is **intentionally not supported** and fails
fast at generation time. A custom scalar requires a user-provided Go marshaler
and an explicit `models:` binding in `gqlgen.yml`, which conflicts with this
plugin's zero-config autobind model: the generator binds every GraphQL type to a
pb Go type automatically and owns the generated `gqlgen.yml`. Supporting custom
scalars would force users to hand-maintain marshalers and binding entries that
the plugin regenerates. If you need a custom scalar, model the field as one of
the supported types (or `JSON`) and convert in your service implementation.

### `OneofOptions.input_mode`

A proto input oneof maps to a GraphQL input. The mode controls enforcement:

- `ONEOF_INPUT_UNSPECIFIED` (default) / `ONEOF_DIRECTIVE` — emit
  `input X @oneOf { ... }`; the schema enforces "exactly one" and gqlgen
  guarantees ≤1 variant is populated. The `ToPb<Msg>` shim picks the set variant.
- `ALL_NULLABLE` — emit a **plain** input object (no `@oneOf`, all variant fields
  nullable) and enforce "exactly one set" at **runtime** in the `ToPb<Msg>` shim
  (it returns an error otherwise). This trades schema-level enforcement for broad
  client/tooling compatibility (some clients and codegen do not support `@oneOf`).

Both modes produce a `ToPb<Msg>(...) (*pb.<Msg>, error)` shim; the resolver
wraps a non-nil error via `graphqlpb.GraphQLError`.

See also `docs/oneof.md` for the detailed oneof handling patterns.
