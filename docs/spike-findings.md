# Spike findings — gqlgen v0.17.90 + protoc-gen-go integration

These are the load-bearing facts the generator (Tasks 4.3, 5.1, 5.2, 5.3, 7.1) must
reproduce. Discovered by hand-building `spike/` against the golden proto and running
gqlgen in-process. gqlgen v0.17.90, gqlparser v2.5.33.

## 1. No `resolver:` block → generator owns the resolvers

With **no `resolver:` block** in `gqlgen.yml`, gqlgen emits only the exec engine
(`generated/generated.go`) plus the resolver INTERFACES — no stub files, nothing to
copy-through. The generator emits one resolver type implementing `ResolverRoot` +
the sub-resolver interfaces. This sidesteps the resolver-ownership/idempotency
problem entirely. Re-running gqlgen regenerates only `generated.go` (verified
byte-identical on re-run); the generator's resolver file is never touched.

## 2. The generated resolver interfaces (shape the generator must match)

```go
type ResolverRoot interface {
    Book() BookResolver
    Mutation() MutationResolver
    Query() QueryResolver
    Subscription() SubscriptionResolver
}
type BookResolver interface { // only because tags is forceResolver (see #4)
    Tags(ctx context.Context, obj *gen.Book) (any, error)
}
type MutationResolver interface {
    AddBook(ctx context.Context, input gen.AddBookRequest) (*gen.AddBookResponse, error)
}
type QueryResolver interface {
    GetBook(ctx context.Context, input gen.GetBookRequest) (*gen.GetBookResponse, error)
}
type SubscriptionResolver interface {
    WatchBooks(ctx context.Context, input gen.WatchBooksRequest) (<-chan *gen.Book, error)
}
```

Key signature facts:
- **Inputs are passed BY VALUE** (`gen.GetBookRequest`, not `*gen.GetBookRequest`).
  The resolver delegates with `r.Library.GetBook(ctx, &input)`.
- Output objects are pointers (`*gen.GetBookResponse`).
- Subscription returns `<-chan *gen.Book`.
- Resolver method names are the Go-exported form of the operation field
  (`getBook` → `GetBook`).

## 3. Proto enum binding — alias package + Marshal/Unmarshal funcs

gqlgen cannot bind a GraphQL `enum` directly to a protoc-gen-go enum (`type Genre
int32` with `String()` but no `MarshalGQL`). It emits calls to `_Genre` /
`unmarshalInputGenre` it never defines → compile failure.

Letting gqlgen generate its own `type Genre string` breaks INPUT binding (the pb
request field is `pb.Genre`, not a string), and input fields cannot use a field
resolver.

**Solution (the generator must emit one such file per proto enum):** a small
adapter package exposing a type ALIAS plus name-keyed Marshal/Unmarshal funcs:

```go
package pbgql
type Genre = pb.Genre // alias: SAME type as pb.Genre, so fields bind directly
func MarshalGenre(g pb.Genre) graphql.Marshaler { /* writes strconv.Quote(g.String()) */ }
func UnmarshalGenre(v any) (pb.Genre, error)     { /* pb.Genre(pb.Genre_value[v.(string)]) */ }
```

Bind in `gqlgen.yml`: `Genre: { model: <pkg>/pbgql.Genre }`. gqlgen finds
`pbgql.MarshalGenre`/`pbgql.UnmarshalGenre` by name and uses them; the alias keeps
the field type identical to `pb.Genre` so output AND input bind with no conversion.
Enum value NAMES are the protojson wire form ("FICTION").

## 4. map<K,V> → JSON scalar requires a field resolver

The `JSON` scalar's Go type is `any`. gqlgen's binder rejects `any` against a
concrete `map[string]string` field — even on OUTPUT ("JSON is incompatible with
map[string]string"). Mark the field `@goField(forceResolver: true)`; gqlgen then
emits a field resolver `Tags(ctx, obj *pb.Book) (any, error)` which the generator
implements as `return obj.GetTags(), nil`.

map on the INPUT side is harder (input fields can't have resolvers). For now the
generator should OMIT map fields from input objects (the spike drops `tags` from
`BookInput`). Proper input-map support is a later concern (a typed map scalar).

## 5. `@goField` must be declared in the schema

Programmatic `api.Generate` (unlike the gqlgen CLI) does NOT auto-inject the
builtin `@goField`/`@goModel`/`@goTag` directive definitions. The generated
`schema.graphql` MUST declare any builtin directive it uses, e.g.:
```graphql
directive @goField(forceResolver: Boolean, name: String) on FIELD_DEFINITION | INPUT_FIELD_DEFINITION
```

## 6. The in-process runner must live in its own package dir

`cmd/gqlgenrun` (and the spike's `gqlgenrun/`) must NOT sit in the same directory
as the resolver package — gqlgen's package loader reads all files in the resolver
dir and a `package main` runner there conflicts ("model and resolver define the
same import path with different package names"). The `//go:build ignore` tag does
NOT save you; gqlgen's loader ignores it. Put the runner in its own subdirectory.

Runner body: `config.LoadConfig("gqlgen.yml")` then `api.Generate(cfg)`.

## 8. oneof — proven pattern (oneofprobe/ spike)

Spike directory: `oneofprobe/` (throwaway; removed after generalization).
All four spike tests (output union Hit, output union Miss, input @oneOf text, input @oneOf author) pass.

### 8a. Output oneof → GraphQL union

**Problem**: gqlgen requires each union MEMBER Go type to implement a marker method
`func (T) IsUnionName(){}`. We cannot add that method to `pb.Hit` (generated code).

**Solution — wrapper types in pbgql**:
1. Define the union as a Go **interface** in `pbgql`: `type RespR interface { IsRespR() }`.
   - Must be an interface (not a struct) — gqlgen calls `findGoInterface` on the union model.
2. Define per-variant wrapper structs in `pbgql`:
   ```go
   type HitWrapper  struct{ *pb.Hit  }; func (HitWrapper)  IsRespR() {}
   type MissWrapper struct{ *pb.Miss }; func (MissWrapper) IsRespR() {}
   ```
   Embedding `*pb.X` gives gqlgen access to all fields for selection sets.
3. In `gqlgen.yml` models:
   ```yaml
   RespR: { model: .../pbgql.RespR }
   Hit:   { model: .../pbgql.HitWrapper }
   Miss:  { model: .../pbgql.MissWrapper }
   ```
4. The oneof field in the parent message (`Resp.r`) **must be** `@goField(forceResolver: true)`.
   gqlgen then emits `RespResolver.R(ctx, obj *pb.Resp) (RespR, error)`.
5. The resolver wraps the pb oneof value:
   ```go
   func (r respResolver) R(ctx context.Context, obj *pb.Resp) (pbgql.RespR, error) {
       return pbgql.WrapRespR(obj), nil
   }
   ```

**Why wrappers go in pbgql (not gqlapi)**:
`exec` imports the wrapper types; `gqlapi` imports `exec`. If wrappers were in `gqlapi`,
the cycle would be `exec → gqlapi → exec`. Putting them in `pbgql` breaks the cycle:
`exec → pbgql`, `gqlapi → exec`, `gqlapi → pbgql` — all acyclic.

### 8b. Input oneof → @oneOf input

**Problem**: a proto oneof field (`message Req { oneof q { string text=1; ... } }`) compiles
to `Req.Q isReq_Q` — a Go interface. gqlgen cannot populate an interface field.
Binding `Req` directly to `pb.Req` and using a @oneOf input type for the field won't
work because gqlgen would try to assign a struct to the interface field.

**Solution — intermediate input struct in pbgql**:
1. Declare the `@oneOf` input in schema: `input ReqQ @oneOf { text: String; author: String }`.
2. Declare `@oneOf` directive in schema (same as `@goField` — see finding #5):
   ```graphql
   directive @oneOf on INPUT_OBJECT
   ```
3. Define in `pbgql`:
   ```go
   type ReqQ struct { Text *string; Author *string }
   type ReqInput struct { Q *ReqQ }
   ```
4. In `gqlgen.yml` bind `Req → pbgql.ReqInput`, `ReqQ → pbgql.ReqQ` (NOT to pb types).
5. gqlgen emits: `QueryResolver.Do(ctx, input pbgql.ReqInput) (*pb.Resp, error)`.
6. The resolver converts via a small shim:
   ```go
   func ToPbReq(r *ReqInput) *pb.Req { /* switch on r.Q.Text / r.Q.Author */ }
   ```

This localized shim is **the only place** a non-pb type is used in the resolver path.
All non-oneof types continue to bind directly to pb.

### 8c. Schema for both cases

```graphql
directive @oneOf on INPUT_OBJECT

# Output union
union RespR = Hit | Miss
type Resp { r: RespR @goField(forceResolver: true) }

# Input @oneOf
input ReqQ @oneOf { text: String; author: String }
input Req { q: ReqQ }
```

## 7. copylocks vet warnings are expected and benign

Because gqlgen copies proto messages by value (input args, args map, some
marshalers), `go vet ./...` reports many `copylocks` warnings (proto
`MessageState` embeds a zero-size `sync.Mutex` copy-detection marker). This is the
known proto+gqlgen reality:
- `go build` and `go test ./...` are UNAFFECTED (copylocks is not in the `go test`
  default vet subset; verified `go test ./...` exits 0).
- Runtime is safe (the lock is `[0]sync.Mutex`, zero size).
Do NOT make `go vet ./...` a generation gate. Document for consumers. (A future
improvement could make inputs nullable→pointer to reduce the input-side copies.)
