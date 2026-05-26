# protoc-gen-go-graphql

A `protoc` plugin that generates a GraphQL API over an existing gRPC service.
Browser clients get a typed, subscription-capable API powered by
[gqlgen](https://github.com/99designs/gqlgen) with zero hand-written transport
code. The proto types (`*pb.*`) are the single source of truth: generated GraphQL
types bind directly to the `protoc-gen-go` structs via gqlgen `autobind` — there
is no second model set and no proto-to-model converter layer. Subscriptions work
over WebSocket using the `graphql-transport-ws` protocol (compatible with Apollo
Client and urql).

---

## Install

The plugin and its two companions are declared as Go tool dependencies in
`go.mod`:

```go
tool (
    github.com/gopherex/protoc-gen-go-graphql
    google.golang.org/grpc/cmd/protoc-gen-go-grpc
    google.golang.org/protobuf/cmd/protoc-gen-go
)
```

Build the plugin binaries:

```sh
go build -o bin/protoc-gen-go-graphql github.com/gopherex/protoc-gen-go-graphql
go build -o bin/protoc-gen-go         google.golang.org/protobuf/cmd/protoc-gen-go
go build -o bin/protoc-gen-go-grpc    google.golang.org/grpc/cmd/protoc-gen-go-grpc
```

Or use the repo's `Makefile`:

```sh
make build
```

---

## Generation

Generation happens in two phases. Run them in order.

### Phase A — protoc

Run `protoc` with all three plugins. This produces the pb types, gRPC stubs, the
GraphQL schema, gqlgen config, and a delegating resolver. The canonical example
is the `gen-test` Makefile target:

```sh
protoc -I example/ -I . -I /usr/include \
    --plugin=protoc-gen-go=bin/protoc-gen-go \
    --plugin=protoc-gen-go-grpc=bin/protoc-gen-go-grpc \
    --plugin=protoc-gen-go-graphql=bin/protoc-gen-go-graphql \
    --go_out=example/gen      --go_opt=paths=source_relative \
    --go-grpc_out=example/gen --go-grpc_opt=paths=source_relative \
    --go-graphql_out=example/gen --go-graphql_opt=paths=source_relative \
    example/golden.proto
```

The `-I /usr/include` flag (controlled by `WKT_INC` in the Makefile) points to
the directory containing the well-known-type `.proto` files
(`google/protobuf/*.proto`). Override if your protoc installation places them
elsewhere:

```sh
make gen-test WKT_INC=/usr/local/include
```

### Phase B — gqlgen

After the protoc run, run `go generate` to let gqlgen load the now-on-disk pb
package, autobind types, and emit the execution engine:

```sh
cd example/gen && go generate ./...
# or from the repo root:
make gen-test   # runs both phases
```

This invokes `cmd/gqlgenrun` (via the `//go:generate` directive in
`gqlapi/generate.go`), which calls `config.LoadConfig("gqlgen.yml")` then
`api.Generate(cfg)` in-process. The result is `gqlapi/exec/exec.go` — the
gqlgen execution engine with `NewExecutableSchema` and all resolver interfaces.

> **Why two phases?** gqlgen's binder must type-check the pb package to emit
> field-access code. The pb package does not exist on disk until protoc finishes,
> so gqlgen cannot run during the protoc pass. See `docs/mapping.md` for details.

---

## Wiring the generated server

After generation, wire the execution engine to your gRPC implementation and an
HTTP handler. The pattern mirrors `example/server_test.go`:

```go
import (
    "github.com/99designs/gqlgen/graphql/handler"
    "github.com/99designs/gqlgen/graphql/handler/transport"
    "yourmodule/gen/gqlapi"
    "yourmodule/gen/gqlapi/exec"
)

// yourImpl implements the generated pb.YourServiceServer interface.
schema := exec.NewExecutableSchema(exec.Config{
    Resolvers: &gqlapi.Resolver{YourService: yourImpl},
})

srv := handler.New(schema)
srv.AddTransport(transport.POST{})       // standard HTTP POST for queries/mutations
srv.AddTransport(transport.Websocket{})  // graphql-transport-ws for subscriptions

http.Handle("/graphql", srv)
```

For queries and mutations without subscriptions, `handler.NewDefaultServer` is
sufficient (it sets up POST + GET transports). Add `transport.Websocket{}` only
when you need subscriptions.

The `Resolver` struct holds one field per gRPC service, typed as the generated
`pb.XxxServer` interface. Assign your implementation directly — the generated
resolvers delegate to it with no conversion.

---

## Supported / unsupported

| Feature | Status |
|---|---|
| Unary rpc `NO_SIDE_EFFECTS` → Query | Supported |
| Unary rpc `IDEMPOTENT` / `IDEMPOTENCY_UNKNOWN` → Mutation | Supported |
| Server-streaming rpc → Subscription | Supported |
| Output oneof → GraphQL union | Supported |
| Input oneof → `@oneOf` input | Supported |
| `map<K,V>` on output fields | Supported (JSON scalar) |
| `map<K,V>` on input fields | Not supported in v1 (field omitted) |
| **bidi-streaming rpc** | **Hard generation error** |
| **client-streaming rpc** | **Hard generation error** |

Bidi-streaming and client-streaming RPCs are intentionally unsupported: GraphQL
subscriptions are server-to-client only, and bridging full-duplex streams would
require a custom WebSocket transport outside the scope of this plugin (v1).

---

## `go vet` and copylocks warnings

Because gqlgen passes proto request messages by value (input args), `go vet ./...`
reports `copylocks` warnings (`proto.MessageState` embeds a zero-size lock
marker). `go build` and `go test ./...` are unaffected — the vet warning does not
block compilation or tests, and the runtime is safe (zero-size lock). Do not use
`go vet ./...` as a generation gate.

---

## References

- `example/` — worked example: `golden.proto` (Library service with query,
  mutation, subscription, oneof, map, WKT) + `server_test.go` (full round-trip
  tests including `TestWireRoundTrip`, `TestSubscription`, and all four oneof
  tests).
- `docs/mapping.md` — full proto→GraphQL mapping table, two-phase model,
  command sequence, output layout.
- `docs/oneof.md` — detailed oneof patterns (output union wrapper types, input
  `@oneOf` intermediate struct, `ToPb*` shim).
