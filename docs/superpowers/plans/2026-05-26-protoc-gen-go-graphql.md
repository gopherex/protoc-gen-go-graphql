# protoc-gen-go-graphql Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `protoc` plugin that generates a gqlgen-based GraphQL API over an existing gRPC service, binding GraphQL types directly to `*pb.*` Go types (no converter layer).

**Architecture:** Two-phase generation. Phase A (our plugin, in the protoc run) emits `schema.graphql`, `gqlgen.yml`, delegating resolvers, runtime scalar bindings, and a `//go:generate` directive. Phase B (`go generate`, in-process `api.Generate`) runs gqlgen after `*.pb.go` is on disk, autobinding GraphQL types to pb structs. Built spike-first: hand-write the proven target output, then build the generator to reproduce it.

**Tech Stack:** Go 1.25, `google.golang.org/protobuf` (protogen), `github.com/99designs/gqlgen`, `github.com/vektah/gqlparser/v2`, `google.golang.org/grpc`, `google.golang.org/protobuf/encoding/protojson`.

**Spec:** `docs/superpowers/specs/2026-05-26-protoc-gen-go-graphql-design.md`

---

## File Structure

| Path | Responsibility |
|------|----------------|
| `main.go` | protoc plugin entrypoint: protogen wiring, flag parse, `FEATURE_PROTO3_OPTIONAL` |
| `cmd/gqlgenrun/main.go` | Phase-B runner: `config.LoadConfig` + `api.Generate` |
| `graphqlopt/graphql.proto` | Custom proto options (extend File/Service/Method/Message/Field/Enum/Oneof options) |
| `graphqlopt/graphql.pb.go` | Generated from `graphql.proto` |
| `generator/generator.go` | Orchestrator: routes plugin → per-file generation |
| `generator/settings.go` | `--go-graphql_opt` flag parsing |
| `generator/fail.go` | bidi/client-stream detection → error |
| `generator/naming.go` | GraphQL name derivation (types, inputs, unions, ops, scalars) |
| `generator/schema.go` | proto descriptors → GraphQL SDL string |
| `generator/scalars.go` | scalar selection per field type + gqlgen.yml model entries |
| `generator/gqlgenyml.go` | emit `gqlgen.yml` (autobind, models, exec path, resolver layout) |
| `generator/resolvers.go` | emit delegating resolver `.go` |
| `generator/gogenerate.go` | emit `*_gqlgen_gen.go` with `//go:generate` |
| `runtime/scalars.go` | protojson-aligned scalar marshalers (Int64/UInt64/Bytes/Timestamp/Duration/JSON) |
| `runtime/stream.go` | gRPC server-stream → channel pump + server-stream shim |
| `runtime/errors.go` | gRPC status → `*gqlerror.Error` |
| `example/golden.proto` | full-exercise golden proto |
| `example/negative/*.proto` | bidi + client-stream protos (generation must fail) |
| `example/gen/` | generated output (committed for inspection + tests) |
| `example/server_test.go` | wire round-trip + subscription tests vs fake gRPC |
| `docs/mapping.md`, `docs/oneof.md` | mapping reference docs |
| `Makefile` | `build`, `gen-opts`, `gen-test`, `run-test` |

**Build order:** scaffold → hand-written spike (proves the whole gqlgen integration) → productionize runtime → options proto → plugin skeleton → SDL gen → gqlgen.yml gen → resolver gen → phase-B wiring → end-to-end golden → oneof → negative tests + docs.

---

## Milestone 0: Scaffold & dependencies

### Task 0.1: Pin dependencies and tool binaries

**Files:**
- Modify: `go.mod`
- Create: `tools.go`

- [ ] **Step 1: Add dependencies**

Run:
```bash
cd /home/yaroher/devel/github/gopherex/protoc-gen-go-graphql
go get github.com/99designs/gqlgen@latest
go get github.com/vektah/gqlparser/v2@latest
go get google.golang.org/protobuf@latest
go get google.golang.org/grpc@latest
```

- [ ] **Step 2: Record the resolved gqlgen + gqlparser versions**

Run: `go list -m github.com/99designs/gqlgen github.com/vektah/gqlparser/v2`
Expected: two version lines. Record them in a comment at the top of `cmd/gqlgenrun/main.go` later. These versions are the ground truth for every gqlgen API call in this plan.

- [ ] **Step 3: Create `tools.go` to keep codegen binaries in the module graph**

```go
//go:build tools

package tools

import (
	_ "github.com/99designs/gqlgen"
	_ "google.golang.org/grpc/cmd/protoc-gen-go-grpc"
	_ "google.golang.org/protobuf/cmd/protoc-gen-go"
)
```

- [ ] **Step 4: Tidy and verify it builds**

Run: `go mod tidy && go build ./...`
Expected: no errors (no non-test Go files yet besides tools.go, which is build-tagged out).

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum tools.go
git commit -m "chore: pin gqlgen, protobuf, grpc dependencies"
```

### Task 0.2: Makefile with build + codegen targets

**Files:**
- Create: `Makefile`

- [ ] **Step 1: Write the Makefile**

```makefile
CURDIR := $(shell pwd)
EXAMPLE_DIR := $(CURDIR)/example
EXAMPLE_OUT := $(EXAMPLE_DIR)/gen
OPTS_DIR := $(CURDIR)/graphqlopt

.PHONY: build
build:
	go build -o $(CURDIR)/bin/protoc-gen-go-graphql ./
	go build -o $(CURDIR)/bin/protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go
	go build -o $(CURDIR)/bin/protoc-gen-go-grpc google.golang.org/grpc/cmd/protoc-gen-go-grpc

.PHONY: gen-opts
gen-opts: build
	protoc -I $(OPTS_DIR) -I $(CURDIR) \
		--plugin=protoc-gen-go=$(CURDIR)/bin/protoc-gen-go \
		--go_out=$(CURDIR) --go_opt=paths=source_relative \
		$(OPTS_DIR)/graphql.proto

.PHONY: gen-test
gen-test: build
	rm -rf $(EXAMPLE_OUT) && mkdir -p $(EXAMPLE_OUT)
	protoc -I $(EXAMPLE_DIR) -I $(CURDIR) \
		--plugin=protoc-gen-go=$(CURDIR)/bin/protoc-gen-go \
		--plugin=protoc-gen-go-grpc=$(CURDIR)/bin/protoc-gen-go-grpc \
		--plugin=protoc-gen-go-graphql=$(CURDIR)/bin/protoc-gen-go-graphql \
		--go_out=$(EXAMPLE_OUT) --go_opt=paths=source_relative \
		--go-grpc_out=$(EXAMPLE_OUT) --go-grpc_opt=paths=source_relative \
		--go-graphql_out=$(EXAMPLE_OUT) --go-graphql_opt=paths=source_relative \
		$(EXAMPLE_DIR)/golden.proto
	cd $(EXAMPLE_OUT) && go generate ./...

.PHONY: run-test
run-test:
	go clean -testcache && go test ./...
```

- [ ] **Step 2: Verify protoc is available**

Run: `protoc --version`
Expected: `libprotoc 3.x` or higher. If missing, stop and install protoc before continuing.

- [ ] **Step 3: Commit**

```bash
git add Makefile
git commit -m "chore: add Makefile build and codegen targets"
```

---

## Milestone 1: Hand-written spike (resolves all gqlgen verification gates)

> This milestone hand-writes the exact output the generator must later reproduce, for the core golden proto (no oneof yet). It proves: autobind to pb, custom scalars, delegating resolvers, subscription channel, WS transport, and the resolver-ownership behaviour. Every file here becomes the reference target. Work in a scratch dir `spike/` that mirrors `example/`; it is deleted once the generator reproduces it.

### Task 1.1: Core golden proto + pb/grpc generation

**Files:**
- Create: `example/golden.proto`
- Create (generated): `example/gen/golden.pb.go`, `example/gen/golden_grpc.pb.go`

- [ ] **Step 1: Write `example/golden.proto`**

```proto
syntax = "proto3";

package golden.v1;

option go_package = "github.com/gopherex/protoc-gen-go-graphql/example/gen;gen";

import "google/protobuf/timestamp.proto";

service Library {
  rpc GetBook(GetBookRequest) returns (GetBookResponse) {
    option idempotency_level = NO_SIDE_EFFECTS; // -> Query
  }
  rpc AddBook(AddBookRequest) returns (AddBookResponse); // default -> Mutation
  rpc WatchBooks(WatchBooksRequest) returns (stream Book); // -> Subscription
}

enum Genre {
  GENRE_UNSPECIFIED = 0;
  FICTION = 1;
  NONFICTION = 2;
}

message Author {
  string name = 1;
}

message Book {
  string id = 1;
  string title = 2;
  Genre genre = 3;
  int64 copies = 4;                          // -> String scalar
  bytes cover = 5;                           // -> base64 String
  google.protobuf.Timestamp published_at = 6;// -> Timestamp scalar
  map<string, string> tags = 7;              // -> JSON scalar
  Author author = 8;                         // nested message
}

message GetBookRequest { string id = 1; }
message GetBookResponse { Book book = 1; }
message AddBookRequest { Book book = 1; }
message AddBookResponse { Book book = 1; }
message WatchBooksRequest { Genre genre = 1; }
```

- [ ] **Step 2: Generate pb + grpc only (plugin not built yet)**

Run:
```bash
make build
mkdir -p example/gen
protoc -I example -I . \
  --plugin=protoc-gen-go=./bin/protoc-gen-go \
  --plugin=protoc-gen-go-grpc=./bin/protoc-gen-go-grpc \
  --go_out=example/gen --go_opt=paths=source_relative \
  --go-grpc_out=example/gen --go-grpc_opt=paths=source_relative \
  example/golden.proto
```
Expected: `example/gen/golden.pb.go` and `example/gen/golden_grpc.pb.go` exist.

> NOTE: `make build` will fail at the `protoc-gen-go-graphql` line until Milestone 4. For this task build only the two upstream plugins:
> `go build -o ./bin/protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go && go build -o ./bin/protoc-gen-go-grpc google.golang.org/grpc/cmd/protoc-gen-go-grpc`

- [ ] **Step 3: Verify pb compiles**

Run: `cd example/gen && go build ./...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add example/golden.proto example/gen/golden.pb.go example/gen/golden_grpc.pb.go
git commit -m "test: golden proto + generated pb/grpc stubs"
```

### Task 1.2: runtime scalars (hand-written, with unit tests)

**Files:**
- Create: `runtime/scalars.go`
- Test: `runtime/scalars_test.go`

- [ ] **Step 1: Write failing test for Int64 round-trip**

```go
package runtime

import "testing"

func TestInt64RoundTrip(t *testing.T) {
	const big int64 = 9007199254740993 // > 2^53, loses precision as JSON number
	m := MarshalInt64(big)
	var sb strings.Builder
	m.MarshalGQL(&sb)
	if got := sb.String(); got != `"9007199254740993"` {
		t.Fatalf("marshal = %s, want quoted string", got)
	}
	out, err := UnmarshalInt64("9007199254740993")
	if err != nil || out != big {
		t.Fatalf("unmarshal = %d, %v", out, err)
	}
}
```
(add `import ("strings"; "testing")`)

- [ ] **Step 2: Run, expect fail**

Run: `go test ./runtime/ -run TestInt64RoundTrip`
Expected: FAIL — undefined `MarshalInt64`.

- [ ] **Step 3: Implement scalars**

```go
package runtime

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Int64 (also used for sint64/sfixed64): protojson encodes 64-bit ints as strings.
func MarshalInt64(v int64) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		_, _ = io.WriteString(w, strconv.Quote(strconv.FormatInt(v, 10)))
	})
}

func UnmarshalInt64(v any) (int64, error) {
	switch x := v.(type) {
	case string:
		return strconv.ParseInt(x, 10, 64)
	case json.Number:
		return strconv.ParseInt(x.String(), 10, 64)
	case int64:
		return x, nil
	default:
		return 0, fmt.Errorf("Int64 must be a string, got %T", v)
	}
}

func MarshalUInt64(v uint64) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		_, _ = io.WriteString(w, strconv.Quote(strconv.FormatUint(v, 10)))
	})
}

func UnmarshalUInt64(v any) (uint64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("UInt64 must be a string, got %T", v)
	}
	return strconv.ParseUint(s, 10, 64)
}

// Bytes: protojson encodes as standard base64 string.
func MarshalBytes(v []byte) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		_, _ = io.WriteString(w, strconv.Quote(base64.StdEncoding.EncodeToString(v)))
	})
}

func UnmarshalBytes(v any) ([]byte, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("Bytes must be a base64 string, got %T", v)
	}
	return base64.StdEncoding.DecodeString(s)
}

// Timestamp: protojson RFC3339 with nanos, e.g. "2006-01-02T15:04:05.999999999Z".
func MarshalTimestamp(v *timestamppb.Timestamp) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		if v == nil {
			_, _ = io.WriteString(w, "null")
			return
		}
		_, _ = io.WriteString(w, strconv.Quote(v.AsTime().UTC().Format(time.RFC3339Nano)))
	})
}

func UnmarshalTimestamp(v any) (*timestamppb.Timestamp, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("Timestamp must be an RFC3339 string, got %T", v)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, err
	}
	return timestamppb.New(t), nil
}

// Duration: protojson string with trailing "s", e.g. "1.000340012s".
func MarshalDuration(v *durationpb.Duration) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		if v == nil {
			_, _ = io.WriteString(w, "null")
			return
		}
		_, _ = io.WriteString(w, strconv.Quote(v.AsDuration().String()))
	})
}

func UnmarshalDuration(v any) (*durationpb.Duration, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("Duration must be a string, got %T", v)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return nil, err
	}
	return durationpb.New(d), nil
}

// JSON: maps, Struct, Value, Any, ListValue. Stored as Go map/any per protojson.
func MarshalJSON(v any) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		_ = json.NewEncoder(w).Encode(v)
	})
}

func UnmarshalJSON(v any) (any, error) { return v, nil }
```

> GATE check: `runtime.MarshalJSON`/`UnmarshalJSON` use `any` as the map Go type. protoc-gen-go maps are `map[string]string` etc., not `any`. The map binding is resolved in Task 1.5 (the map field gets a `goField:forceResolver` or a dedicated typed scalar). Keep the JSON scalar generic here.

- [ ] **Step 4: Run, expect pass**

Run: `go test ./runtime/ -run TestInt64RoundTrip`
Expected: PASS.

- [ ] **Step 5: Add Timestamp + Bytes round-trip tests, run, pass**

```go
func TestTimestampRoundTrip(t *testing.T) {
	ts := timestamppb.New(time.Unix(1700000000, 123456789).UTC())
	var sb strings.Builder
	MarshalTimestamp(ts).MarshalGQL(&sb)
	got := strings.Trim(sb.String(), `"`)
	back, err := UnmarshalTimestamp(got)
	if err != nil || !back.AsTime().Equal(ts.AsTime()) {
		t.Fatalf("roundtrip mismatch: %v / %v", back, err)
	}
}

func TestBytesRoundTrip(t *testing.T) {
	in := []byte{0x00, 0x01, 0xff}
	var sb strings.Builder
	MarshalBytes(in).MarshalGQL(&sb)
	out, err := UnmarshalBytes(strings.Trim(sb.String(), `"`))
	if err != nil || string(out) != string(in) {
		t.Fatalf("roundtrip mismatch: %v / %v", out, err)
	}
}
```
Run: `go test ./runtime/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add runtime/scalars.go runtime/scalars_test.go
git commit -m "feat: protojson-aligned graphql custom scalars"
```

### Task 1.3: runtime stream pump + server-stream shim

**Files:**
- Create: `runtime/stream.go`
- Test: `runtime/stream_test.go`

- [ ] **Step 1: Write failing test using a fake stream**

```go
package runtime

import (
	"context"
	"testing"
)

func TestStreamServerSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan *int, 2)
	ss := NewStreamServer[int](ctx, ch)
	v := 7
	if err := ss.Send(&v); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := <-ch; *got != 7 {
		t.Fatalf("got %d", *got)
	}
	cancel()
	w := 8
	if err := ss.Send(&w); err == nil {
		t.Fatalf("send after cancel should error")
	}
}
```

- [ ] **Step 2: Run, expect fail**

Run: `go test ./runtime/ -run TestStreamServerSend`
Expected: FAIL — undefined `NewStreamServer`.

- [ ] **Step 3: Implement**

```go
package runtime

import (
	"context"

	"google.golang.org/grpc"
)

// StreamServer is a generic gRPC server-streaming shim whose Send pushes into a
// channel. It satisfies any pb `Svc_MethodServer` interface (which embeds
// grpc.ServerStream and adds Send(*T) error) when T is the streamed message.
type StreamServer[T any] struct {
	grpc.ServerStream
	ctx context.Context
	ch  chan<- *T
}

func NewStreamServer[T any](ctx context.Context, ch chan<- *T) *StreamServer[T] {
	return &StreamServer[T]{ctx: ctx, ch: ch}
}

func (s *StreamServer[T]) Context() context.Context { return s.ctx }

func (s *StreamServer[T]) Send(m *T) error {
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case s.ch <- m:
		return nil
	}
}

// PumpServerStream runs a server-streaming gRPC method into a channel returned to
// a gqlgen subscription resolver. `start` invokes the gRPC method with the shim.
func PumpServerStream[T any](ctx context.Context, start func(ss *StreamServer[T]) error) <-chan *T {
	ch := make(chan *T)
	go func() {
		defer close(ch)
		_ = start(NewStreamServer[T](ctx, ch))
	}()
	return ch
}
```

- [ ] **Step 4: Run, expect pass**

Run: `go test ./runtime/ -run TestStreamServerSend`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add runtime/stream.go runtime/stream_test.go
git commit -m "feat: server-stream to channel pump for subscriptions"
```

### Task 1.4: runtime gRPC-status → GraphQL error

**Files:**
- Create: `runtime/errors.go`
- Test: `runtime/errors_test.go`

- [ ] **Step 1: Write failing test**

```go
package runtime

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGraphQLErrorCode(t *testing.T) {
	in := status.Error(codes.NotFound, "book missing")
	gqlErr := GraphQLError(context.Background(), in)
	if gqlErr.Message != "book missing" {
		t.Fatalf("message = %q", gqlErr.Message)
	}
	if gqlErr.Extensions["code"] != "NOT_FOUND" {
		t.Fatalf("code = %v", gqlErr.Extensions["code"])
	}
}
```

- [ ] **Step 2: Run, expect fail**

Run: `go test ./runtime/ -run TestGraphQLErrorCode`
Expected: FAIL — undefined `GraphQLError`.

- [ ] **Step 3: Implement**

```go
package runtime

import (
	"context"

	"github.com/vektah/gqlparser/v2/gqlerror"
	"google.golang.org/grpc/status"
)

// GraphQLError maps a gRPC status error to a GraphQL error: codes.Code name into
// extensions["code"], status message as the GraphQL message, and any string
// details into extensions["details"].
func GraphQLError(ctx context.Context, err error) *gqlerror.Error {
	if err == nil {
		return nil
	}
	st, _ := status.FromError(err)
	gqlErr := gqlerror.WrapPath(graphqlPath(ctx), err)
	gqlErr.Message = st.Message()
	gqlErr.Extensions = map[string]any{"code": st.Code().String()}
	if d := st.Details(); len(d) > 0 {
		details := make([]string, 0, len(d))
		for _, item := range d {
			details = append(details, toString(item))
		}
		gqlErr.Extensions["details"] = details
	}
	return gqlErr
}
```

> GATE check: `st.Code().String()` returns Go-cased names like `NotFound`, not `NOT_FOUND`. The test expects `NOT_FOUND`. Implement an explicit `codes.Code -> SCREAMING_SNAKE` map (`codeName(st.Code())`) and use it instead of `.String()`. Add helpers `graphqlPath(ctx)` (use `graphql.GetPath(ctx)` from gqlgen, or return nil if unavailable) and `toString(any)` (fmt.Sprintf "%v"). Adjust imports accordingly.

- [ ] **Step 4: Implement the gate fix, run, expect pass**

Run: `go test ./runtime/ -run TestGraphQLErrorCode`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add runtime/errors.go runtime/errors_test.go
git commit -m "feat: gRPC status to GraphQL error mapping"
```

### Task 1.5: Hand-write schema.graphql + gqlgen.yml + delegating resolvers; prove gqlgen runs

**Files:**
- Create: `spike/schema.graphql`
- Create: `spike/gqlgen.yml`
- Create: `spike/resolver.go`
- Create: `spike/gqlgen_run.go`
- (Generated by gqlgen): `spike/generated/*`

> This is the keystone task. It hand-writes the target output and runs gqlgen in-process against the already-generated pb package, resolving every verification gate. `spike/` imports `example/gen` (the pb package).

- [ ] **Step 1: Write `spike/schema.graphql`**

```graphql
scalar Int64
scalar Bytes
scalar Timestamp
scalar JSON

enum Genre { GENRE_UNSPECIFIED FICTION NONFICTION }

type Author { name: String! }

type Book {
  id: String!
  title: String!
  genre: Genre!
  copies: Int64!
  cover: Bytes!
  publishedAt: Timestamp
  tags: JSON
  author: Author
}

input GetBookRequest { id: String! }
input AddBookRequest { book: BookInput }
input BookInput {
  id: String!
  title: String!
  genre: Genre!
  copies: Int64!
  cover: Bytes!
  publishedAt: Timestamp
  tags: JSON
  author: AuthorInput
}
input AuthorInput { name: String! }
input WatchBooksRequest { genre: Genre! }

type GetBookResponse { book: Book }
type AddBookResponse { book: Book }

type Query { getBook(input: GetBookRequest!): GetBookResponse! }
type Mutation { addBook(input: AddBookRequest!): AddBookResponse! }
type Subscription { watchBooks(input: WatchBooksRequest!): Book! }
```

> GATE: GraphQL forbids a type and input sharing a name. `Book` (output) and `BookInput` (input) are distinct types. Output types bind to `*pb.Book`; input types ALSO bind to `*pb.Book` via `models:` (same Go struct, two GraphQL names). Confirm gqlgen permits two GraphQL types pointing at one Go model — it does, model binding is per GraphQL type. This is the name-collision rule from spec §11.5: inputs get an `Input` suffix.

- [ ] **Step 2: Write `spike/gqlgen.yml`**

```yaml
schema:
  - schema.graphql

exec:
  package: generated
  filename: generated/generated.go

# We bind to pb; do NOT let gqlgen generate its own models.
autobind:
  - github.com/gopherex/protoc-gen-go-graphql/example/gen

models:
  Int64:     { model: github.com/gopherex/protoc-gen-go-graphql/runtime.Int64 }
  Bytes:     { model: github.com/gopherex/protoc-gen-go-graphql/runtime.Bytes }
  Timestamp: { model: github.com/gopherex/protoc-gen-go-graphql/runtime.Timestamp }
  JSON:      { model: github.com/gopherex/protoc-gen-go-graphql/runtime.JSON }
  Genre:     { model: github.com/gopherex/protoc-gen-go-graphql/example/gen.Genre }
  Book:      { model: github.com/gopherex/protoc-gen-go-graphql/example/gen.Book }
  BookInput: { model: github.com/gopherex/protoc-gen-go-graphql/example/gen.Book }
  Author:    { model: github.com/gopherex/protoc-gen-go-graphql/example/gen.Author }
  AuthorInput: { model: github.com/gopherex/protoc-gen-go-graphql/example/gen.Author }
  GetBookRequest:   { model: github.com/gopherex/protoc-gen-go-graphql/example/gen.GetBookRequest }
  GetBookResponse:  { model: github.com/gopherex/protoc-gen-go-graphql/example/gen.GetBookResponse }
  AddBookRequest:   { model: github.com/gopherex/protoc-gen-go-graphql/example/gen.AddBookRequest }
  AddBookResponse:  { model: github.com/gopherex/protoc-gen-go-graphql/example/gen.AddBookResponse }
  WatchBooksRequest: { model: github.com/gopherex/protoc-gen-go-graphql/example/gen.WatchBooksRequest }

resolver:
  layout: follow-schema
  dir: .
  package: spike
  filename_template: "{name}.resolvers.go"
```

> GATE: scalar model values (`runtime.Int64` etc.) require a Go TYPE named `Int64` with `MarshalGQL`/`UnmarshalGQL`, OR a pair of `MarshalInt64`/`UnmarshalInt64` funcs. Task 1.2 wrote funcs `MarshalInt64`/`UnmarshalInt64`. gqlgen binds a scalar to a package path + name and looks for `Marshal<Name>`/`Unmarshal<Name>` functions. So the model value must be the BARE NAME the funcs are suffixed with: `runtime.Int64` → gqlgen looks for `runtime.MarshalInt64`/`runtime.UnmarshalInt64`. Confirm this convention in the gqlgen version pinned in Task 0.1 (scalars reference doc). If gqlgen instead requires a named type, add `type Int64 = int64` etc. with methods. Resolve here by running gqlgen (Step 5) and reading the error if any.

- [ ] **Step 3: Hand-write `spike/resolver.go` (delegating resolver root + non-generated methods)**

```go
package spike

import (
	"context"

	pb "github.com/gopherex/protoc-gen-go-graphql/example/gen"
	"github.com/gopherex/protoc-gen-go-graphql/runtime"
)

// Resolver holds the gRPC server implementation and delegates to it.
type Resolver struct {
	Library pb.LibraryServer
}

func (r *Resolver) getBook(ctx context.Context, input *pb.GetBookRequest) (*pb.GetBookResponse, error) {
	resp, err := r.Library.GetBook(ctx, input)
	if err != nil {
		return nil, runtime.GraphQLError(ctx, err)
	}
	return resp, nil
}

func (r *Resolver) addBook(ctx context.Context, input *pb.AddBookRequest) (*pb.AddBookResponse, error) {
	resp, err := r.Library.AddBook(ctx, input)
	if err != nil {
		return nil, runtime.GraphQLError(ctx, err)
	}
	return resp, nil
}

func (r *Resolver) watchBooks(ctx context.Context, input *pb.WatchBooksRequest) (<-chan *pb.Book, error) {
	return runtime.PumpServerStream[pb.Book](ctx, func(ss *runtime.StreamServer[pb.Book]) error {
		return r.Library.WatchBooks(input, ss)
	}), nil
}
```

> GATE (resolver ownership): gqlgen with `resolver.layout: follow-schema` generates resolver method STUBS into `{name}.resolvers.go`, and re-running gqlgen only ADDS missing methods (does not overwrite existing ones). The signatures must match exactly. Approach: let gqlgen generate the stubs ONCE (Step 5), then replace each stub body with a one-line delegation to the helper methods above (e.g. `return r.getBook(ctx, input)`). The generator (Milestone 7) reproduces the post-edit file directly. Verify in Step 6 that re-running gqlgen does not duplicate methods.

- [ ] **Step 4: Write `spike/gqlgen_run.go` (in-process generate entrypoint)**

```go
//go:build ignore

package main

import (
	"fmt"
	"os"

	"github.com/99designs/gqlgen/api"
	"github.com/99designs/gqlgen/codegen/config"
)

func main() {
	cfg, err := config.LoadConfig("gqlgen.yml")
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(2)
	}
	if err := api.Generate(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
}
```

> GATE: confirm `config.LoadConfig(path string) (*config.Config, error)` and `api.Generate(cfg *config.Config, ...api.Option) error` signatures against the pinned version. Adjust if the version differs (e.g. older `LoadConfigFromDefaultLocations`). These two calls are the entire phase-B runtime.

- [ ] **Step 5: Run gqlgen against the spike**

Run:
```bash
cd spike && go run gqlgen_run.go
```
Expected: creates `spike/generated/generated.go` and `spike/*.resolvers.go` stubs. If it errors on scalars or autobind, apply the gate fixes in Steps 2 above and re-run. Record exact fixes in `docs/oneof.md` notes section for the generator to reproduce.

- [ ] **Step 6: Wire the generated resolver stubs to delegation, prove it compiles, prove re-run is idempotent**

Edit the generated `spike/*.resolvers.go` so each resolver method body calls the matching delegation helper (`r.getBook`, `r.addBook`, `r.watchBooks`). Then:
Run: `cd spike && go run gqlgen_run.go && go build ./...`
Expected: gqlgen adds no duplicate methods (idempotent); `go build` PASS. This confirms the resolver-ownership gate.

- [ ] **Step 7: Commit the spike as the reference target**

```bash
git add runtime/ spike/
git commit -m "spike: hand-written gqlgen target proving autobind, scalars, resolvers"
```

### Task 1.6: Wire round-trip + subscription test against fake gRPC

**Files:**
- Create: `spike/server_test.go`

- [ ] **Step 1: Write the wire round-trip test**

```go
package spike

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/99designs/gqlgen/client"
	"github.com/99designs/gqlgen/graphql/handler"
	pb "github.com/gopherex/protoc-gen-go-graphql/example/gen"
	"github.com/gopherex/protoc-gen-go-graphql/spike/generated"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeLibrary struct {
	pb.UnimplementedLibraryServer
	book *pb.Book
}

func (f *fakeLibrary) GetBook(_ context.Context, _ *pb.GetBookRequest) (*pb.GetBookResponse, error) {
	return &pb.GetBookResponse{Book: f.book}, nil
}

func TestWireRoundTrip(t *testing.T) {
	want := &pb.Book{
		Id: "b1", Title: "T", Genre: pb.Genre_FICTION,
		Copies:      9007199254740993, // > 2^53
		Cover:       []byte{0, 1, 255},
		PublishedAt: timestamppb.New(timeFixed()),
	}
	srv := handler.NewDefaultServer(generated.NewExecutableSchema(generated.Config{
		Resolvers: &Resolver{Library: &fakeLibrary{book: want}},
	}))
	c := client.New(srv)

	var resp struct {
		GetBook struct {
			Book struct {
				ID, Title, Genre, Copies, Cover, PublishedAt string
			}
		}
	}
	c.MustPost(`{ getBook(input:{id:"b1"}) { book { id title genre copies cover publishedAt } } }`, &resp)

	// protojson is the canonical wire: compare GraphQL output to protojson of pb.
	pj, _ := protojson.Marshal(want)
	var pjMap map[string]any
	_ = json.Unmarshal(pj, &pjMap)
	if resp.GetBook.Book.Copies != pjMap["copies"].(string) {
		t.Fatalf("copies: gql=%s protojson=%v", resp.GetBook.Book.Copies, pjMap["copies"])
	}
	if resp.GetBook.Book.Genre != "FICTION" {
		t.Fatalf("genre = %s", resp.GetBook.Book.Genre)
	}
}
```
(add a `timeFixed()` helper returning a fixed `time.Time`.)

- [ ] **Step 2: Run, expect pass (or fix scalar/binding issues surfaced)**

Run: `cd spike && go test ./... -run TestWireRoundTrip`
Expected: PASS. Byte-compatibility on int64 (string) and enum (name) confirmed against protojson.

- [ ] **Step 3: Add subscription test**

```go
func TestSubscription(t *testing.T) {
	books := []*pb.Book{{Id: "1"}, {Id: "2"}}
	srv := handler.New(generated.NewExecutableSchema(generated.Config{
		Resolvers: &Resolver{Library: &streamFake{books: books}},
	}))
	srv.AddTransport(transport.Websocket{})
	srv.AddTransport(transport.POST{})
	// Use client.New(srv).Websocket(...) to read two messages and assert ids "1","2".
}
```
Implement `streamFake.WatchBooks(req, stream)` to `stream.Send` each book then return nil. Use gqlgen's `client.Websocket` helper to read the subscription stream.

> GATE: confirm `transport.Websocket{}` import path and the gqlgen test `client.Websocket` API against the pinned version. This proves the WS/subscription path end to end.

- [ ] **Step 4: Run, expect pass**

Run: `cd spike && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add spike/server_test.go
git commit -m "test: wire round-trip + subscription against fake gRPC"
```

---

## Milestone 2: Options proto

### Task 2.1: Define and generate graphql.proto options

**Files:**
- Create: `graphqlopt/graphql.proto`
- Create (generated): `graphqlopt/graphql.pb.go`

- [ ] **Step 1: Write `graphqlopt/graphql.proto`**

```proto
syntax = "proto3";

package graphqlopt;

option go_package = "github.com/gopherex/protoc-gen-go-graphql/graphqlopt;graphqlopt";

import "google/protobuf/descriptor.proto";

extend google.protobuf.FileOptions   { FileOptions    file    = 79000; }
extend google.protobuf.ServiceOptions { ServiceOptions service = 79000; }
extend google.protobuf.MethodOptions { MethodOptions  method  = 79000; }
extend google.protobuf.MessageOptions { MessageOptions message = 79000; }
extend google.protobuf.FieldOptions  { FieldOptions   field   = 79000; }
extend google.protobuf.EnumOptions   { EnumOptions    enum    = 79000; }
extend google.protobuf.OneofOptions  { OneofOptions   oneof   = 79000; }

message FileOptions {
  bool generate = 1;
  string pb_package = 2;        // import path for autobind
  string schema_filename = 3;   // default schema.graphql
  string gqlgen_config_filename = 4; // default gqlgen.yml
  string exec_package = 5;
  string exec_filename = 6;
}

enum Operation {
  OPERATION_UNSPECIFIED = 0; // use idempotency_level
  QUERY = 1;
  MUTATION = 2;
  SUBSCRIPTION = 3;
  SKIP = 4;
}

message ServiceOptions {
  bool skip = 1;
  string name_prefix = 2;
}

message MethodOptions {
  Operation operation = 1;   // override idempotency-based default
  string operation_name = 2; // override field name
}

message MessageOptions {
  string name = 1;
  bool skip = 2;
}

enum OneofInputMode {
  ONEOF_INPUT_UNSPECIFIED = 0; // use file/generator default
  ONEOF_DIRECTIVE = 1;         // @oneOf
  ALL_NULLABLE = 2;            // all-nullable input object + runtime check
}

message FieldOptions {
  string name = 1;
  bool exclude = 2;
  string scalar = 3; // override scalar binding
}

message EnumOptions { string name = 1; }

message OneofOptions {
  string union_name = 1;
  OneofInputMode input_mode = 2;
}
```

- [ ] **Step 2: Generate the pb.go**

Run: `make gen-opts`
Expected: `graphqlopt/graphql.pb.go` created.

> NOTE: `make gen-opts` depends on `make build` which builds `protoc-gen-go-graphql`. Until Milestone 4 that binary does not exist; for this task build only `protoc-gen-go` and run the `protoc` line from the `gen-opts` target manually.

- [ ] **Step 3: Verify it compiles**

Run: `go build ./graphqlopt/`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add graphqlopt/graphql.proto graphqlopt/graphql.pb.go
git commit -m "feat: graphql options proto extensions"
```

---

## Milestone 3: Plugin skeleton

### Task 3.1: main.go + settings

**Files:**
- Create: `main.go`
- Create: `generator/settings.go`
- Create: `generator/generator.go`

- [ ] **Step 1: Write `generator/settings.go`**

```go
package generator

import "flag"

type Settings struct {
	Paths string // source_relative etc., passed through to protoc convention
}

func (s *Settings) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&s.Paths, "paths", "", "paths mode (source_relative)")
}
```

- [ ] **Step 2: Write `generator/generator.go` (skeleton)**

```go
package generator

import "google.golang.org/protobuf/compiler/protogen"

type Generator struct {
	Plugin   *protogen.Plugin
	Settings *Settings
}

func New(p *protogen.Plugin, s *Settings) *Generator {
	return &Generator{Plugin: p, Settings: s}
}

func (g *Generator) Generate() error {
	for _, f := range g.Plugin.Files {
		if !f.Generate {
			continue
		}
		if err := g.generateFile(f); err != nil {
			return err
		}
	}
	return nil
}

// generateFile is implemented across schema.go, gqlgenyml.go, resolvers.go.
func (g *Generator) generateFile(f *protogen.File) error {
	return nil // filled in later milestones
}
```

- [ ] **Step 3: Write `main.go`**

```go
package main

import (
	"flag"

	"github.com/gopherex/protoc-gen-go-graphql/generator"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/types/pluginpb"
)

func main() {
	var flags flag.FlagSet
	s := &generator.Settings{}
	s.RegisterFlags(&flags)

	protogen.Options{ParamFunc: flags.Set}.Run(func(p *protogen.Plugin) error {
		p.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)
		return generator.New(p, s).Generate()
	})
}
```

- [ ] **Step 4: Build the plugin**

Run: `go build -o ./bin/protoc-gen-go-graphql ./`
Expected: PASS.

- [ ] **Step 5: Smoke-run against golden (produces nothing yet)**

Run: `make gen-test` (the `go generate` sub-step may no-op; ignore for now)
Expected: protoc exits 0; no graphql files produced yet.

- [ ] **Step 6: Commit**

```bash
git add main.go generator/settings.go generator/generator.go
git commit -m "feat: plugin entrypoint and generator skeleton"
```

### Task 3.2: Fail-fast on bidi / client-streaming

**Files:**
- Create: `generator/fail.go`
- Modify: `generator/generator.go`
- Test: `generator/fail_test.go`
- Create: `example/negative/bidi.proto`, `example/negative/client_stream.proto`

- [ ] **Step 1: Write `example/negative/bidi.proto` and `client_stream.proto`**

```proto
// bidi.proto
syntax = "proto3";
package negative.v1;
option go_package = "github.com/gopherex/protoc-gen-go-graphql/example/negative/gen;gen";
service Chat { rpc Talk(stream Msg) returns (stream Msg); }
message Msg { string text = 1; }
```
```proto
// client_stream.proto
syntax = "proto3";
package negative.v1;
option go_package = "github.com/gopherex/protoc-gen-go-graphql/example/negative/gen;gen";
service Upload { rpc Send(stream Chunk) returns (Ack); }
message Chunk { bytes data = 1; }
message Ack { bool ok = 1; }
```

- [ ] **Step 2: Write failing test**

```go
package generator

import (
	"strings"
	"testing"
)

func TestCheckStreamingRejectsBidi(t *testing.T) {
	// methodStub mimics the streaming flags of a protogen.Method.
	err := checkStreaming("Chat", "Talk", true /*client*/, true /*server*/)
	if err == nil || !strings.Contains(err.Error(), "bidi") {
		t.Fatalf("want bidi error, got %v", err)
	}
}

func TestCheckStreamingRejectsClientStream(t *testing.T) {
	err := checkStreaming("Upload", "Send", true, false)
	if err == nil || !strings.Contains(err.Error(), "client-streaming") {
		t.Fatalf("want client-stream error, got %v", err)
	}
}

func TestCheckStreamingAllowsServerStream(t *testing.T) {
	if err := checkStreaming("Library", "WatchBooks", false, true); err != nil {
		t.Fatalf("server stream should be allowed: %v", err)
	}
}
```

- [ ] **Step 3: Run, expect fail**

Run: `go test ./generator/ -run TestCheckStreaming`
Expected: FAIL — undefined `checkStreaming`.

- [ ] **Step 4: Implement `generator/fail.go`**

```go
package generator

import "fmt"

// checkStreaming returns a fail-fast error for unsupported streaming shapes.
func checkStreaming(service, method string, client, server bool) error {
	switch {
	case client && server:
		return fmt.Errorf("%s.%s: bidi-streaming rpc is not supported (GraphQL subscriptions are server->client only)", service, method)
	case client:
		return fmt.Errorf("%s.%s: client-streaming rpc is not supported", service, method)
	default:
		return nil
	}
}
```

- [ ] **Step 5: Run, expect pass**

Run: `go test ./generator/ -run TestCheckStreaming`
Expected: PASS.

- [ ] **Step 6: Wire into generateFile**

In `generator/generator.go`, before emitting anything, iterate services/methods and call `checkStreaming(svc.GoName, m.GoName, m.Desc.IsStreamingClient(), m.Desc.IsStreamingServer())`; return the first error.

- [ ] **Step 7: Prove negative protos fail generation**

Run:
```bash
go build -o ./bin/protoc-gen-go-graphql ./
protoc -I example/negative -I . \
  --plugin=protoc-gen-go-graphql=./bin/protoc-gen-go-graphql \
  --go-graphql_out=/tmp/neg example/negative/bidi.proto
```
Expected: protoc fails with the bidi error message. Repeat for `client_stream.proto`.

- [ ] **Step 8: Commit**

```bash
git add generator/fail.go generator/fail_test.go example/negative/
git commit -m "feat: fail-fast on bidi and client-streaming rpc"
```

---

## Milestone 4: SDL generation

### Task 4.1: Naming helpers

**Files:**
- Create: `generator/naming.go`
- Test: `generator/naming_test.go`

- [ ] **Step 1: Write failing tests**

```go
package generator

import "testing"

func TestFieldName(t *testing.T) {
	if got := fieldName("published_at"); got != "publishedAt" {
		t.Fatalf("got %q", got)
	}
}

func TestInputName(t *testing.T) {
	if got := inputName("Book"); got != "BookInput" {
		t.Fatalf("got %q", got)
	}
}

func TestOperationFieldName(t *testing.T) {
	if got := operationFieldName("GetBook"); got != "getBook" {
		t.Fatalf("got %q", got)
	}
}
```

- [ ] **Step 2: Run, expect fail**

Run: `go test ./generator/ -run 'TestFieldName|TestInputName|TestOperationFieldName'`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
package generator

import "strings"

// fieldName converts proto snake_case to GraphQL camelCase.
func fieldName(proto string) string {
	parts := strings.Split(proto, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

func inputName(typeName string) string { return typeName + "Input" }

func operationFieldName(rpcName string) string {
	if rpcName == "" {
		return rpcName
	}
	return strings.ToLower(rpcName[:1]) + rpcName[1:]
}
```

- [ ] **Step 4: Run, expect pass**

Run: `go test ./generator/ -run 'TestFieldName|TestInputName|TestOperationFieldName'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add generator/naming.go generator/naming_test.go
git commit -m "feat: GraphQL naming helpers"
```

### Task 4.2: Scalar selection per proto field

**Files:**
- Create: `generator/scalars.go`
- Test: `generator/scalars_test.go`

- [ ] **Step 1: Write failing test**

```go
package generator

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestScalarForKind(t *testing.T) {
	cases := map[protoreflect.Kind]string{
		protoreflect.StringKind: "String",
		protoreflect.BoolKind:   "Boolean",
		protoreflect.Int32Kind:  "Int",
		protoreflect.Int64Kind:  "Int64",
		protoreflect.Uint64Kind: "Int64", // bound via UInt64 marshaler, GraphQL type Int64
		protoreflect.BytesKind:  "Bytes",
		protoreflect.DoubleKind: "Float",
	}
	for k, want := range cases {
		if got := scalarForKind(k); got != want {
			t.Fatalf("kind %v: got %q want %q", k, got, want)
		}
	}
}
```

- [ ] **Step 2: Run, expect fail**

Run: `go test ./generator/ -run TestScalarForKind`
Expected: FAIL.

- [ ] **Step 3: Implement (well-known type and map handling come in schema.go)**

```go
package generator

import "google.golang.org/protobuf/reflect/protoreflect"

// scalarForKind maps a scalar proto kind to a GraphQL scalar name.
// Message/enum/group kinds are handled by the caller (named types).
func scalarForKind(k protoreflect.Kind) string {
	switch k {
	case protoreflect.BoolKind:
		return "Boolean"
	case protoreflect.StringKind:
		return "String"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "Int"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "Int64"
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return "Float"
	case protoreflect.BytesKind:
		return "Bytes"
	default:
		return ""
	}
}
```

- [ ] **Step 4: Run, expect pass**

Run: `go test ./generator/ -run TestScalarForKind`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add generator/scalars.go generator/scalars_test.go
git commit -m "feat: proto kind to GraphQL scalar mapping"
```

### Task 4.3: SDL generation from descriptors (golden comparison)

**Files:**
- Create: `generator/schema.go`
- Modify: `generator/generator.go`
- Test: `generator/schema_test.go`
- Create: `generator/testdata/golden.schema.graphql`

- [ ] **Step 1: Save the expected SDL as golden**

Copy the proven `spike/schema.graphql` (from Task 1.5) to `generator/testdata/golden.schema.graphql` — that is the exact target the generator must produce for `example/golden.proto`.

- [ ] **Step 2: Write the golden test**

```go
package generator

import (
	"os"
	"testing"
)

func TestBuildSchemaMatchesGolden(t *testing.T) {
	file := loadTestProto(t, "testdata/golden.proto") // helper compiles proto to *protogen.File
	got := buildSchema(file)
	want, _ := os.ReadFile("testdata/golden.schema.graphql")
	if normalize(got) != normalize(string(want)) {
		t.Fatalf("schema mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
```
`loadTestProto` uses `protodesc`/`protoparse` (via `github.com/jhump/protoreflect` OR by invoking protoc to a FileDescriptorSet and decoding) to build a `*protogen.File`. `normalize` trims trailing whitespace and blank lines. Add `generator/testdata/golden.proto` = copy of `example/golden.proto`.

> If building a `*protogen.File` in a unit test is heavy, instead drive the whole plugin in an integration test: run the built plugin via protoc against `testdata/golden.proto` and compare the emitted `schema.graphql`. Choose the integration approach if `protogen.File` construction proves awkward — note the decision in the test file comment.

- [ ] **Step 3: Run, expect fail**

Run: `go test ./generator/ -run TestBuildSchema`
Expected: FAIL — undefined `buildSchema`.

- [ ] **Step 4: Implement `buildSchema`**

Implement `buildSchema(f *protogen.File) string` in `generator/schema.go`. It must emit, in order:
1. `scalar` declarations for each custom scalar actually used (Int64, Bytes, Timestamp, Duration, JSON) — track usage while walking fields.
2. each enum as `enum Name { VALUE ... }` (value names = proto names).
3. each message as a `type Name { field: GqlType ... }` (output).
4. an `input NameInput { ... }` for each message reachable as an rpc request (and nested input messages).
5. `Query`, `Mutation`, `Subscription` blocks built from services/methods:
   - operation chosen by `methodOperation(m)` = explicit option override → else idempotency: `NO_SIDE_EFFECTS`→Query, else Mutation; server-stream→Subscription.
   - field: `operationFieldName(m.GoName)(input: <ReqInput>!): <Resp>!` (Query/Mutation) or `: <StreamType>!` (Subscription).

Field type rules: scalar via `scalarForKind`; message → named type; enum → enum name; `map<K,V>` → `JSON`; WKT Timestamp→`Timestamp`, Duration→`Duration`, Struct/Value/Any/ListValue→`JSON`. Nullability: proto3 singular message fields → nullable; scalars with `optional` → nullable; repeated → `[T!]`; required-ish (non-optional scalar) → `T!`. Match the golden exactly.

Add `methodOperation(m *protogen.Method) operation` reading `graphqlopt.MethodOptions` + `m.Desc.Options()` idempotency.

- [ ] **Step 5: Iterate until golden matches**

Run: `go test ./generator/ -run TestBuildSchema`
Expected: PASS. Adjust emission until it equals `testdata/golden.schema.graphql`.

- [ ] **Step 6: Wire into generateFile + emit the file**

In `generateFile`, after the streaming check, call `buildSchema(f)` and write it via `g.Plugin.NewGeneratedFile("schema.graphql", "")`. Verify via `make gen-test` that `example/gen/schema.graphql` matches the golden.

- [ ] **Step 7: Commit**

```bash
git add generator/schema.go generator/schema_test.go generator/testdata/ generator/generator.go
git commit -m "feat: generate GraphQL SDL from proto descriptors"
```

---

## Milestone 5: gqlgen.yml + resolver + go:generate emission

### Task 5.1: Emit gqlgen.yml

**Files:**
- Create: `generator/gqlgenyml.go`
- Modify: `generator/generator.go`
- Test: `generator/gqlgenyml_test.go`
- Create: `generator/testdata/golden.gqlgen.yml`

- [ ] **Step 1: Save proven `spike/gqlgen.yml` to `generator/testdata/golden.gqlgen.yml`**

Use the exact gqlgen.yml proven in Task 1.5 (with the pb import path `github.com/gopherex/protoc-gen-go-graphql/example/gen`).

- [ ] **Step 2: Write golden test**

```go
func TestBuildGqlgenYmlMatchesGolden(t *testing.T) {
	file := loadTestProto(t, "testdata/golden.proto")
	got := buildGqlgenYml(file, "github.com/gopherex/protoc-gen-go-graphql/example/gen")
	want, _ := os.ReadFile("testdata/golden.gqlgen.yml")
	if normalize(got) != normalize(string(want)) {
		t.Fatalf("gqlgen.yml mismatch:\n%s", got)
	}
}
```

- [ ] **Step 3: Run, expect fail**

Run: `go test ./generator/ -run TestBuildGqlgenYml`
Expected: FAIL.

- [ ] **Step 4: Implement `buildGqlgenYml(f, pbImport)`**

Emit: `schema:` list; `exec:` package/filename; `autobind:` = [pbImport]; `models:` = scalar bindings to `runtime` + one entry per message/enum/input → pb type (input names map to the base pb type). `resolver:` block with `layout: follow-schema`, dir, package, filename_template. Derive `pbImport` from the file's `go_package` or the `graphqlopt.FileOptions.pb_package` override.

- [ ] **Step 5: Run, expect pass**

Run: `go test ./generator/ -run TestBuildGqlgenYml`
Expected: PASS.

- [ ] **Step 6: Emit in generateFile + verify via make gen-test**

Write `gqlgen.yml` via `NewGeneratedFile`. Run `make gen-test`; confirm `example/gen/gqlgen.yml` matches golden.

- [ ] **Step 7: Commit**

```bash
git add generator/gqlgenyml.go generator/gqlgenyml_test.go generator/testdata/golden.gqlgen.yml generator/generator.go
git commit -m "feat: generate gqlgen.yml with autobind and scalar bindings"
```

### Task 5.2: Emit delegating resolver file

**Files:**
- Create: `generator/resolvers.go`
- Modify: `generator/generator.go`
- Test: `generator/resolvers_test.go`
- Create: `generator/testdata/golden.resolvers.go.txt`

- [ ] **Step 1: Save proven resolver as golden**

Save the proven delegation code (the post-edit `spike/*.resolvers.go` wired to `r.getBook`/`r.addBook`/`r.watchBooks`, plus `spike/resolver.go` helpers) to `generator/testdata/golden.resolvers.go.txt`. The generator emits ONE file combining the resolver struct + all method bodies (delegation), so gqlgen's resolvergen finds every method already present and adds none.

> KEY: the generator emits complete resolver method implementations (not stubs), so phase-B gqlgen never overwrites them. Confirmed idempotent in Task 1.5 Step 6.

- [ ] **Step 2: Write golden test**

```go
func TestBuildResolversMatchesGolden(t *testing.T) {
	file := loadTestProto(t, "testdata/golden.proto")
	got := buildResolvers(file, "github.com/gopherex/protoc-gen-go-graphql/example/gen")
	want, _ := os.ReadFile("testdata/golden.resolvers.go.txt")
	if normalize(got) != normalize(string(want)) {
		t.Fatalf("resolvers mismatch:\n%s", got)
	}
}
```

- [ ] **Step 3: Run, expect fail; implement `buildResolvers`; iterate to pass**

`buildResolvers(f, pbImport)` emits: package clause, imports (pb, runtime, context), `Resolver` struct holding one field per service (`Library pb.LibraryServer`), and one method per rpc:
- Query/Mutation: `func (r *Resolver) <op>(ctx, input *pb.<Req>) (*pb.<Resp>, error)` calling the server and wrapping errors with `runtime.GraphQLError`.
- Subscription: `func (r *Resolver) <op>(ctx, input *pb.<Req>) (<-chan *pb.<T>, error)` using `runtime.PumpServerStream`.

Match method names to what gqlgen's generated interface expects (lowercase-first field name → exported resolver method; confirm casing from the spike's generated interface). Run `go test ./generator/ -run TestBuildResolvers` until PASS.

- [ ] **Step 4: Emit in generateFile**

Write `<prefix>.graphql_resolvers.go` via `NewGeneratedFile` with the pb `GoImportPath`.

- [ ] **Step 5: Commit**

```bash
git add generator/resolvers.go generator/resolvers_test.go generator/testdata/golden.resolvers.go.txt generator/generator.go
git commit -m "feat: generate delegating gRPC resolvers"
```

### Task 5.3: Emit //go:generate runner + cmd/gqlgenrun

**Files:**
- Create: `cmd/gqlgenrun/main.go`
- Create: `generator/gogenerate.go`
- Modify: `generator/generator.go`

- [ ] **Step 1: Write `cmd/gqlgenrun/main.go` (from the proven spike runner)**

Use the proven Task 1.4 entrypoint, without the `//go:build ignore` tag:
```go
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/99designs/gqlgen/api"
	"github.com/99designs/gqlgen/codegen/config"
)

func main() {
	cfgPath := flag.String("config", "gqlgen.yml", "path to gqlgen.yml")
	flag.Parse()
	cfg, err := config.LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(2)
	}
	if err := api.Generate(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
}
```

- [ ] **Step 2: Implement `buildGoGenerate` in `generator/gogenerate.go`**

Emit a file `<prefix>_gqlgen_gen.go`:
```go
package <pkg>

//go:generate go run github.com/gopherex/protoc-gen-go-graphql/cmd/gqlgenrun --config gqlgen.yml
```
Use `g.Plugin.NewGeneratedFile` with the pb `GoImportPath`; package name comes from the pb file's `GoPackageName`.

- [ ] **Step 3: Wire into generateFile + verify build**

Run: `go build ./...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/gqlgenrun/main.go generator/gogenerate.go generator/generator.go
git commit -m "feat: emit go:generate directive and in-process gqlgen runner"
```

---

## Milestone 6: End-to-end golden

### Task 6.1: Full pipeline reproduces the spike and passes wire tests

**Files:**
- Modify: `example/server_test.go` (moved from `spike/server_test.go`, repointed to `example/gen`)
- Delete: `spike/`

- [ ] **Step 1: Run the full generator pipeline**

Run: `make gen-test`
Expected: produces in `example/gen/`: `golden.pb.go`, `golden_grpc.pb.go`, `schema.graphql`, `gqlgen.yml`, `golden.graphql_resolvers.go`, `golden_gqlgen_gen.go`, and (after the `go generate` sub-step) the gqlgen exec package. Everything compiles.

- [ ] **Step 2: Diff generated output against the proven spike**

Compare `example/gen/schema.graphql`, `gqlgen.yml`, and resolver file to the Task-1 spike artifacts. They must be equivalent (modulo the `spike` vs `gen` package name). Fix generator emission for any difference.

- [ ] **Step 3: Move the wire round-trip + subscription tests to example**

Move `spike/server_test.go` to `example/server_test.go`, change package + imports to `example/gen` generated exec package. Run:
Run: `go test ./example/...`
Expected: PASS (wire round-trip + subscription).

- [ ] **Step 4: Delete the spike**

Run: `git rm -r spike`
The spike has served its purpose; the generator now reproduces it.

- [ ] **Step 5: Verify clean regeneration from scratch**

Run: `rm -rf example/gen && make gen-test && go test ./example/...`
Expected: PASS from a clean state — proves the two-phase flow end to end.

- [ ] **Step 6: Commit**

```bash
git add example/ && git rm -r --cached spike 2>/dev/null; git add -A
git commit -m "test: end-to-end golden generation + wire/subscription tests"
```

---

## Milestone 7: oneof (output union + @oneOf input)

### Task 7.1: Extend golden proto with oneof

**Files:**
- Modify: `example/golden.proto`
- Modify: `generator/testdata/golden.proto` + golden output files

- [ ] **Step 1: Add a oneof rpc to the golden proto**

```proto
// in service Library:
rpc SearchBooks(SearchRequest) returns (SearchResponse) {
  option idempotency_level = NO_SIDE_EFFECTS;
}

message SearchRequest {
  oneof query {        // input oneof -> @oneOf
    string text = 1;
    string author = 2;
  }
}
message SearchResponse {
  oneof result {       // output oneof -> union of object variants
    Book book = 1;
    NotFound not_found = 2;
  }
}
message NotFound { string reason = 1; }
```

- [ ] **Step 2: Regenerate pb and confirm the spike binding by hand first**

Hand-extend `generator/testdata/golden.schema.graphql` with:
```graphql
input SearchRequest @oneOf { text: String author: String }
union SearchResult = Book | NotFound
type NotFound { reason: String! }
type SearchResponse { result: SearchResult }
```
(Query gains `searchBooks(input: SearchRequest!): SearchResponse!`.)

> GATE: validate `@oneOf` against the pinned gqlparser/gqlgen by running the spike runner on this schema. If `@oneOf` is rejected, switch to ALL_NULLABLE mode: emit `input SearchRequest { text: String author: String }` (no directive) and add a runtime exactly-one check in the resolver. Record the outcome in `docs/oneof.md`. The generator must support both modes via `OneofOptions.input_mode`.

- [ ] **Step 3: Output union resolution**

protoc-gen-go represents `oneof result` as an interface field `Result isSearchResponse_Result`. gqlgen needs a type resolver to pick the union member. Add to the generated resolver a `SearchResponse_result` resolver (or a union type resolver) mapping the pb oneof wrapper (`*SearchResponse_Book`, `*SearchResponse_NotFound`) to `*pb.Book` / `*pb.NotFound`. Hand-write this in the spike, prove it compiles + resolves, then encode into the generator.

- [ ] **Step 4: Update generator emission for unions + @oneOf**

Extend `buildSchema` to detect oneofs: output oneof → `union` + per-message variants (reject scalar variants with a clear error per spec §6, or wrap — document choice in `docs/oneof.md`); input oneof → `@oneOf` input (or all-nullable per mode). Extend `buildResolvers` for the union type resolver. Update golden files and re-run `go test ./generator/...`.

- [ ] **Step 5: Wire test for oneof**

Add to `example/server_test.go`: a `SearchBooks` query returning a `Book` variant, asserting the union resolves to the `Book` selection set; and one returning `NotFound`.
Run: `make gen-test && go test ./example/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add example/golden.proto generator/ example/server_test.go docs/oneof.md
git commit -m "feat: oneof support (output union + @oneOf input)"
```

---

## Milestone 8: Docs

### Task 8.1: Mapping and oneof docs + README

**Files:**
- Create: `docs/mapping.md`
- Modify: `docs/oneof.md`
- Create: `README.md`

- [ ] **Step 1: Write `docs/mapping.md`**

Reproduce the spec §6 mapping table, plus the two-phase generation explanation (spec §4) and the end-user command sequence (`protoc ...` then `go generate ./...`).

- [ ] **Step 2: Finalize `docs/oneof.md`**

Document the chosen input mode (`@oneOf` or all-nullable per the Task 7.2 gate outcome), output union rules, and scalar-variant handling.

- [ ] **Step 3: Write `README.md`**

Quickstart: install, the protoc invocation with `--go-graphql_out`, the `go generate` phase, wiring the resolver to a gRPC impl, adding the WS transport for subscriptions. Reference `example/` as the worked example.

- [ ] **Step 4: Commit**

```bash
git add docs/ README.md
git commit -m "docs: mapping reference, oneof, and README quickstart"
```

---

## Self-Review (completed during planning)

**Spec coverage:**
- §2 hard rule (direct-bind, no converters) → Tasks 1.5, 5.1 (autobind/models), no converter tasks anywhere. ✓
- §3 decisions: layout (M0-M8 structure), gqlgen in-process (1.4, 5.3), map=JSON (1.2, 4.2), @oneOf+fallback (7.1 gate), output union (7.1), bidi/client-stream hard error (3.2). ✓
- §4 two-phase → 1.4, 1.5, 5.3, 6.1. ✓
- §6 mapping table → 4.2 (scalars), 4.3 (schema), 1.2 (WKT/64-bit/bytes/JSON). ✓
- §7 pipeline → 3.x, 4.x, 5.x. ✓
- §8 resolvers → 1.3, 1.5, 5.2. ✓
- §9 scalars → 1.2. ✓
- §10 options proto → 2.1. ✓
- §11 gates → embedded as GATE callouts in 1.2, 1.4, 1.5, 1.6, 7.1. ✓
- §12 acceptance → 6.1 (compile + clean regen), 1.6/6.1 (wire round-trip), 3.2 (negative), 5.1 (no converters). ✓

**Placeholder scan:** GATE callouts defer specific gqlgen API confirmations to the spike (Milestone 1), which surfaces them via real compilation errors with documented fallbacks — not silent TODOs. No "implement later" without code.

**Type consistency:** `MarshalInt64`/`UnmarshalInt64`, `StreamServer[T]`/`PumpServerStream[T]`, `GraphQLError(ctx, err)`, `checkStreaming`, `buildSchema`/`buildGqlgenYml`/`buildResolvers`/`buildGoGenerate`, `fieldName`/`inputName`/`operationFieldName`/`scalarForKind` used consistently across tasks.
