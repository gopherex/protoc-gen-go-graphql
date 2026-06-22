# protoc-gen-go-graphql

A `protoc` plugin that generates a GraphQL API over an existing gRPC service in a
**single protoc pass**. It emits one self-contained `schema.go` per package that
builds an executable [`*graphql.Schema`](https://github.com/graphql-go/graphql)
whose field resolvers delegate **directly to your `pb.*ServiceServer`
implementations** — no second model set, no hand-written transport, no codegen
second phase.

Every GraphQL field gets an explicitly generated resolver, so there is no
autobind and no resolver/schema drift: the proto types (`*pb.*`) are the single
source of truth, and the generated resolvers call the same gRPC method
implementations your gRPC/Connect handlers already use.

---

## Install

```sh
make build
# builds bin/protoc-gen-go-graphql (+ protoc-gen-go, protoc-gen-go-grpc)
```

Or install the plugin by pinned version:

```sh
go install github.com/gopherex/protoc-gen-go-graphql@latest
```

The generated code depends on the small runtime package
`github.com/gopherex/protoc-gen-go-graphql/graphqlrt` (custom scalars,
subscription stream pump, gRPC-status → GraphQL error mapping) and on
`github.com/graphql-go/graphql`.

---

## Generation

One `protoc` invocation — no `go generate`, no second phase:

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

`-I /usr/include` (`WKT_INC` in the Makefile) points at the well-known-type
`.proto` files. The canonical example is `make gen-test`.

Files in the same Go package aggregate into one `gqlapi/schema.go`.

### Plugin flags

Pass via `--go-graphql_opt=<flag>=<value>`:

| Flag | Default | Description |
|---|---|---|
| `paths` | _(empty)_ | Set to `source_relative` for source-relative output paths. |
| `out_dir` | `gqlapi` | Subpackage directory + Go package name for the generated code. |

---

## Wiring

Build the schema with a `Server` bound to your gRPC implementations and serve it
with any GraphQL HTTP handler:

```go
import (
    graphqlhandler "github.com/graphql-go/handler"
    "yourmodule/gen/gqlapi"
)

schema, err := gqlapi.NewSchema(&gqlapi.Server{
    YourService: yourImpl, // implements pb.YourServiceServer
    // ... one field per service ...

    // Optional: enforce per-method authz on the in-process GraphQL path. Called
    // before each operation delegates; return the (identity-enriched) context.
    Authorize: func(ctx context.Context, procedure string, req interface{}) (context.Context, error) {
        return ctx, nil
    },
})

http.Handle("/graphql", graphqlhandler.New(&graphqlhandler.Config{Schema: &schema}))
```

`Server` holds one field per gRPC service, typed as the generated
`pb.XxxServiceServer` interface — assign your implementation directly; the
generated resolvers delegate to it with no conversion. Because resolvers run the
gRPC method **in-process** (no transport), apply authn/authz via the `Authorize`
hook (bridge the credential into `ctx` before execution).

Subscriptions (server-streaming RPCs) are exposed as `graphql.Field.Subscribe`
returning a channel (`graphqlrt.PumpServerStream`); serve them with a websocket
transport of your choice.

---

## Supported / unsupported

| Feature | Status |
|---|---|
| Unary rpc `NO_SIDE_EFFECTS` → Query | Supported |
| Unary rpc `IDEMPOTENT` / `IDEMPOTENCY_UNKNOWN` → Mutation | Supported |
| Server-streaming rpc → Subscription | Supported (`Field.Subscribe` channel) |
| Output oneof → GraphQL union | Supported |
| Input oneof → `@oneOf`-style input | Supported |
| `map<K,V>`, `Struct`/`Value`/`Any` → JSON scalar | Supported |
| 64-bit ints, bytes, Timestamp, Duration → custom scalars | Supported (protojson-aligned) |
| `(graphqlopt.service\|method).skip` | Supported (omits from the GraphQL surface) |
| **bidi-streaming / client-streaming rpc** | **Hard generation error** (skip them) |

---

## References

- `example/` — `golden.proto` (Library service: query, mutation, subscription,
  oneof, map, WKT, optional, ALL_NULLABLE) + `gen/gqlapi/schema_manual_test.go`
  (round-trip query + subscription against a fake server); `multipkg/`
  (cross-package response types).
- `docs/mapping.md` — proto → GraphQL mapping reference.
- `docs/oneof.md` — oneof (union / `@oneOf` input) handling.
