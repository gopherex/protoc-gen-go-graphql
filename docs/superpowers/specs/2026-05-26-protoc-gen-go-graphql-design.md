# protoc-gen-go-graphql — Design

Date: 2026-05-26
Status: Approved (pre-implementation)
Module: `github.com/gopherex/protoc-gen-go-graphql`
Layout reference: `protoc-gen-ogen` (sibling plugin; structure mirrored, converter machinery dropped)

## 1. Goal

A `protoc` plugin that, from annotated `.proto`, generates a GraphQL API over an
existing gRPC service. Browser clients get a universal, typed,
subscription-capable API with zero hand-written transport.

- Server: generated GraphQL schema + a [gqlgen](https://github.com/99designs/gqlgen)
  execution engine whose resolvers delegate to the gRPC service implementation.
- Streaming: GraphQL subscriptions over `graphql-transport-ws` (gqlgen WS
  transport server-side; Apollo/urql/graphql-ws client-side).
- Frontend: standard graphql-code-generator + Apollo/urql. No custom TS generator
  in scope.

## 2. Hard rule — proto types are the single source of truth

Generated GraphQL types bind directly to the `protoc-gen-go` Go types (`*pb.Foo`)
via gqlgen `autobind` + per-type `models:` mapping. There is **no** second model
set and **no** proto↔model converter layer.

Rationale:
1. No type drift, no converter code.
2. Canonical wire = protojson on both ends. Go marshals via
   `google.golang.org/protobuf/encoding/protojson`; the TS client uses protobuf-es
   (`toJson`/`fromJson`). They agree byte-for-byte on the cases that silently
   corrupt otherwise: 64-bit ints as strings, enums as names, Timestamp/Duration
   formats, oneof shape, default-omit, Any.
3. Semantics come from proto, not re-invented in GraphQL SDL.

Proto-specific shapes that do not bind directly get custom scalars / resolver
shims aligned to protojson — never a parallel type tree.

## 3. Locked decisions

| # | Decision | Choice | Notes |
|---|----------|--------|-------|
| 1 | Layout vs `protoc-gen-ogen` | Adapt: mirror dir skeleton, drop `convert/` + converters + adapter | Direct-bind makes converters unnecessary |
| 2 | gqlgen run mode | In-process `api.Generate`, run in **phase B** | Not inside the protoc run (see §4) |
| 3 | `map<K,V>` encoding | Custom JSON scalar (protojson-aligned) | No `[KeyValuePair]` |
| 4 | oneof input encoding | `@oneOf` input directive | **Gate:** confirm gqlgen/gqlparser support; fallback all-nullable input object + runtime check |
| 5 | oneof output encoding | GraphQL `union` | object variants; scalar variants wrapped in 1-field object or rejected |
| 6 | bidi / client-stream rpc | Hard error, fail-fast | No silent skip/degrade |

## 4. Two-phase generation (and why)

gqlgen's binder loads the bound model package (`pb`) via `go/packages` **at
generate time** — it must type-check `pb.User` to emit field-access code
(`obj.Name`, getters, oneof wrappers). It cannot run before `pb` exists on disk.

In one `protoc` run, protoc writes every plugin's output **after all plugins
finish**, so `*.pb.go` is not on disk while our plugin runs. Therefore the gqlgen
pass must run **after** the protoc pass. This is not a hack — it is the standard
gqlgen workflow (models exist first, gqlgen second). `protoc-gen-ogen` avoids this
only because it generates its own models and never loads `pb` at gen time; the
direct-bind hard rule removes that escape hatch.

```
Phase A  protoc run (our plugin among --go_out, --go-grpc_out):
           --go_out        -> *.pb.go
           --go-grpc_out   -> *_grpc.pb.go
           --go-graphql_out-> schema.graphql, gqlgen.yml,
                              *.graphql_resolvers.go (delegating resolvers),
                              *_gqlgen_gen.go (//go:generate -> cmd/gqlgenrun)
Phase B  go generate ./...  (runs cmd/gqlgenrun, in-process):
           config.LoadConfig(gqlgen.yml) -> api.Generate(cfg)
           loads pb (now on disk), autobind, emits exec engine + Resolver
           interface + bound models.
```

The plugin owns only phase A. The user's build (Makefile/buf/easyp/go generate)
owns orchestration. We do **not** auto-run gqlgen from the plugin on
already-present pb files — that path silently uses stale types when the proto
changed in the same edit cycle.

`runtime/` is a fixed library in our module imported by generated code, **not**
regenerated per run.

## 5. Project layout

```
protoc-gen-go-graphql/
  main.go                   protogen entry, flag parse, FEATURE_PROTO3_OPTIONAL
  cmd/gqlgenrun/main.go      phase-B runner: config.LoadConfig + api.Generate
  graphqlopt/
    graphql.proto            extend File/Service/Method/Message/Field/Enum/Oneof Options
    graphql.pb.go            generated (via easyp/protoc, like ogen/ in reference)
  generator/
    generator.go             router -> GraphQLGenerator
    settings.go              --go-graphql_opt flag parsing
    schema.go                proto descriptors -> GraphQL SDL (mapping core)
    scalars.go               scalar decisions + gqlgen.yml model entries
    gqlgenyml.go             emit gqlgen.yml (autobind, models, exec/resolver paths)
    resolvers.go             emit delegating resolver .go
    naming.go                GraphQL name derivation (types, inputs, unions, ops)
    fail.go                  bidi/client-stream detection -> error
  runtime/                   library imported by generated code (not regenerated)
    scalars.go               Int64/UInt64/Bytes/Timestamp/Duration/JSON marshalers
    stream.go                gRPC server-stream -> <-chan *pb.T pump + server-stream shim
    errors.go                grpc status -> *gqlerror.Error (extensions.code, details)
  example/
    golden.proto             unary query + unary mutation + server-stream + enum
                             + nested message + map + WKT
    negative/                bidi.proto, client_stream.proto (generation MUST fail)
    gen/                     pb, grpc, schema.graphql, gqlgen.yml, resolvers, gqlgen engine
    server_test.go           wire round-trip vs fake gRPC impl
  docs/
    mapping.md
    oneof.md
  Makefile                   build, gen-opts, gen-test, run-test
  go.mod
```

## 6. proto -> GraphQL mapping

| proto construct | GraphQL output |
|---|---|
| file (option `generate` on) | `schema.graphql` + `gqlgen.yml` |
| message | output type; request message -> input type |
| enum | enum (member names = proto value names) |
| unary rpc, `NO_SIDE_EFFECTS` | `Query` |
| unary rpc, `IDEMPOTENT` / `IDEMPOTENCY_UNKNOWN` | `Mutation` (overridable by option) |
| server-streaming rpc | `Subscription`; resolver returns `<-chan *pb.T` |
| **bidi-streaming rpc** | **hard generation error** |
| **client-streaming rpc** | **hard generation error** |
| oneof (output) | `union` of object variants (scalar variants wrapped in a 1-field object, or rejected with a clear error) |
| oneof (input) | `@oneOf` input directive (fallback: all-nullable input object + runtime exactly-one check) |
| `map<K,V>` | custom JSON scalar, protojson-aligned |
| `int64`/`uint64`/`fixed64`/`sfixed64` | `String` scalar (protojson encodes 64-bit as strings; GraphQL `Int` is 32-bit) |
| `bytes` | base64 `String` |
| `Timestamp` / `Duration` | custom scalars (protojson string form) |
| `Struct` / `Value` / `Any` / `ListValue` | JSON scalar |
| gRPC `status` | GraphQL error: `extensions.code` = `codes.Code` name, message surfaced, details into extensions |

Operation default chosen by builtin `google.protobuf.MethodOptions.idempotency_level`;
the per-method option only overrides it. No new option re-implements that default.

Field validation (ranges/formats) stays on the proto/gRPC side (e.g.
protovalidate in the gRPC impl). The GraphQL layer validates only structure/types.

## 7. Phase-A pipeline (the plugin)

1. Read descriptors via `google.golang.org/protobuf/compiler/protogen`.
2. **Fail-fast scan:** for each generated service method, if
   `Desc.IsStreamingClient()` (client-stream or bidi) is true, record an error
   carrying the proto source location. Aggregate and return; protoc surfaces it.
   (bidi = streaming client + streaming server; client-stream = streaming client
   only. Both rejected.)
3. Build SDL (`schema.go` + `naming.go`): types, inputs, enums, unions, Query /
   Mutation / Subscription fields, custom scalar declarations.
4. Build `gqlgen.yml` (`gqlgenyml.go` + `scalars.go`): `autobind` to the pb
   import path; per-scalar `models:` bindings to `runtime`; message/enum `models:`
   bindings to pb (autobind covers most, explicit where names collide); exec
   output package/path; resolver layout configured so gqlgen does not overwrite
   our delegating resolvers (see §11 gate).
5. Emit delegating resolver file (`resolvers.go`).
6. Emit `*_gqlgen_gen.go` with `//go:generate go run
   github.com/gopherex/protoc-gen-go-graphql/cmd/gqlgenrun --config gqlgen.yml`.

Plugin declares `FEATURE_PROTO3_OPTIONAL`.

## 8. Resolver delegation (zero conversion)

- The `Resolver` struct holds the gRPC server implementations (the `XxxServer`
  interfaces from `--go-grpc_out`); the user wires their impl in.
- **Query/Mutation resolver:** receives `ctx` + the autobound `*pb.Request`,
  calls `server.Method(ctx, req)`, returns `*pb.Response`. Error ->
  `runtime.GraphQLError`. No conversion — request/response are pb types bound
  directly.
- **Subscription resolver:** returns `(<-chan *pb.T, error)`. `runtime` provides a
  generic server-stream shim:
  ```go
  type streamServer[T any] struct {
      grpc.ServerStream
      ctx context.Context
      ch  chan<- *T
  }
  func (s *streamServer[T]) Send(m *T) error { /* ctx-aware send to ch */ }
  func (s *streamServer[T]) Context() context.Context { return s.ctx }
  ```
  The resolver starts `server.Stream(req, shim)` in a goroutine and returns the
  channel; the channel closes on stream return or `ctx.Done()`.

## 9. Custom scalars (`runtime/scalars.go`)

protojson-aligned `Marshal*`/`Unmarshal*` functions (or `graphql.Marshaler`/
`Unmarshaler` types), registered in `gqlgen.yml` `models:`:

- `Int64` / `UInt64` — 64-bit ints as decimal strings.
- `Bytes` — base64 string.
- `Timestamp` / `Duration` — protojson string form.
- `JSON` — `map<K,V>`, `Struct`, `Value`, `Any`, `ListValue` as JSON.

All must round-trip byte-compatibly with protobuf-es `toJson`/`fromJson`.

## 10. Options proto (`graphqlopt/graphql.proto`)

`extend` on File/Service/Method/Message/Field/Enum/Oneof options. Initial fields:

- **FileOptions:** `generate` (bool), `pb_package` (import path for autobind),
  schema/gqlgen.yml/exec output paths, scalar overrides.
- **ServiceOptions:** include/exclude, name prefix.
- **MethodOptions:** operation override (query/mutation/subscription/skip),
  operation-name override. (`idempotency_level` is builtin; this only overrides.)
- **MessageOptions:** GraphQL name override, treat-as-input.
- **FieldOptions:** name override, exclude, scalar override.
- **EnumOptions:** name override.
- **OneofOptions:** union name override, input-mode override.

Extension field number fixed in the private range (~79000) and documented to avoid
collisions.

## 11. Verification gates (resolve in planning against the installed gqlgen)

These must be confirmed against the pinned gqlgen/gqlparser version, not memory:

1. **`@oneOf` support.** If absent, fall back to all-nullable input object +
   runtime exactly-one validation (decision #4 fallback).
2. **Resolver ownership.** Confirm gqlgen does not overwrite our hand-emitted
   delegating resolvers; resolvergen only adds missing methods, so our method
   signatures must match gqlgen's generated `Resolver` interface exactly.
   Otherwise use a custom resolvergen plugin via `api.AddPlugin`. This is the
   primary integration risk.
3. **Exact entrypoints.** `config.LoadConfig` / `api.Generate` signatures and
   options.
4. **WS wiring.** Exact `transport.Websocket` setup negotiating
   `graphql-transport-ws` (gqlgen supports both `graphql-ws` and
   `graphql-transport-ws`); document the server wiring + the subscription channel
   contract.
5. **autobind name collisions.** A message used as both input and output cannot
   share one Go binding under one GraphQL name; define a naming rule (e.g. input
   suffix) and document it.
6. **protoc-gen-go binding shape.** Confirm gqlgen binds to exported pb fields /
   getters; oneof fields are Go interfaces -> union resolution needs a type
   resolver/shim.

## 12. Acceptance criteria

- One protoc run (`--go_out`, `--go-grpc_out`, `--go-graphql_out`) followed by
  `go generate ./...` produces pb types, gRPC stubs, GraphQL SDL, gqlgen engine,
  delegating resolvers, custom scalars — all compiling.
- The golden `.proto` (unary query NO_SIDE_EFFECTS + unary mutation +
  server-stream subscription + enum + nested message + map + WKT) drives the full
  path; the generated server compiles and resolvers delegate to a fake gRPC impl.
- Wire round-trip: a protobuf-es protojson request -> GraphQL server -> gRPC fake
  -> response -> protobuf-es parse, byte-compatible on 64-bit/enum/Timestamp.
- A bidi rpc and a client-stream rpc (in `example/negative/`) cause explicit
  generation errors, not silent skips.
- No generated proto↔model converter code: GraphQL types bind to `*pb.*` directly;
  only custom scalars adapt proto-specific shapes.

## 13. Out of scope (v1)

- bidi-streaming and client-streaming rpc (GraphQL subscriptions are server->client
  only; would need a separate custom WS transport).
- A custom TypeScript generator (standard graphql-code-generator is used).
- REST/OpenAPI/ogen path.
