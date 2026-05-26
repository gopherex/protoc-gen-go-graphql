# Oneof Handling

Proto `oneof` fields require localized glue code because neither Go variant type
(output: interface fields; input: interface fields) can be bound directly by
gqlgen. This is the **only** place in the generated code where non-pb types
appear in the resolver path. Every other message, enum, and scalar binds directly
to the `*pb.*` types with no intermediate layer.

The patterns below are proven by the `oneofprobe` spike (results captured in
`docs/spike-findings.md` §8) and are reproduced in the `SearchBooks` operation in
`example/golden.proto`.

---

## Output oneof → GraphQL union

### The problem

gqlgen requires each union member's Go type to implement a marker method
`func (T) IsUnionName() {}`. The pb types (`*pb.Book`, `*pb.NotFound`) are
generated code — adding methods to them is not possible.

Additionally, gqlgen requires the Go model for a union to be a **Go interface**
(it calls `findGoInterface` internally). A plain struct or type alias does not
work.

### The solution — wrapper types in `pbgql`

The generator emits the following in `gqlapi/pbgql/<message>_oneof.go`:

1. A union **interface** that gqlgen binds the GraphQL union to:
   ```go
   type SearchResponseResult interface{ IsSearchResponseResult() }
   ```

2. A **wrapper struct** per variant, embedding the pb pointer:
   ```go
   type SearchResponseResultBook     struct{ *pb.Book }
   type SearchResponseResultNotFound struct{ *pb.NotFound }

   func (SearchResponseResultBook)     IsSearchResponseResult() {}
   func (SearchResponseResultNotFound) IsSearchResponseResult() {}
   ```
   Embedding `*pb.X` gives gqlgen full access to all fields for selection sets,
   with no data copying.

3. A **`Wrap*` helper** that converts the pb oneof interface value:
   ```go
   func WrapSearchResponseResult(obj *pb.SearchResponse) SearchResponseResult {
       switch v := obj.GetResult().(type) {
       case *pb.SearchResponse_Book:
           return SearchResponseResultBook{v.Book}
       case *pb.SearchResponse_NotFound:
           return SearchResponseResultNotFound{v.NotFound}
       default:
           return nil
       }
   }
   ```

4. In `gqlgen.yml`, the union and each wrapper are bound to their pbgql types:
   ```yaml
   SearchResponseResult:         { model: .../pbgql.SearchResponseResult }
   SearchResponseResultBook:     { model: .../pbgql.SearchResponseResultBook }
   SearchResponseResultNotFound: { model: .../pbgql.SearchResponseResultNotFound }
   ```

5. The oneof field in the parent message is marked
   `@goField(forceResolver: true)` in the schema. gqlgen then emits a field
   resolver instead of a direct field access:
   ```
   type SearchResponse { result: SearchResponseResult @goField(forceResolver: true) }
   ```

6. The resolver implements the field resolver by calling the wrap helper:
   ```go
   func (r searchResponseResolver) Result(ctx context.Context, obj *pb.SearchResponse) (pbgql.SearchResponseResult, error) {
       return pbgql.WrapSearchResponseResult(obj), nil
   }
   ```

### Why wrappers live in `pbgql`, not `gqlapi`

The gqlgen-generated `exec` package imports the wrapper types (it needs to
reference `pbgql.SearchResponseResultBook` in type switch code). The `gqlapi`
package imports `exec` to satisfy `ResolverRoot`. If the wrappers were in
`gqlapi`, the cycle would be:

```
exec → gqlapi → exec    ← cycle
```

Placing wrappers in `pbgql` breaks the cycle:

```
exec   → pbgql          ← ok
gqlapi → exec           ← ok
gqlapi → pbgql          ← ok
```

---

## Input oneof → `@oneOf` input

### The problem

A proto message with an input oneof (`message SearchRequest { oneof query { string text = 1; string author = 2; } }`)
compiles to a Go interface field: `SearchRequest.Query isSearchRequest_Query`.
gqlgen cannot populate a Go interface field from GraphQL input. Binding
`SearchRequest` directly to `pb.SearchRequest` and using a `@oneOf` input for
the field would fail because gqlgen would attempt to assign a struct value to the
interface field at runtime.

### The solution — intermediate input struct in `pbgql`

The generator emits the following in `gqlapi/pbgql/<message>_oneof.go`:

1. The `@oneOf` input and the wrapper input are declared in `schema.graphql`:
   ```graphql
   input SearchRequestQuery @oneOf {
     text:   String
     author: String
   }
   input SearchRequest { query: SearchRequestQuery }
   ```
   The `@oneOf` directive is declared in the schema header (gqlgen's programmatic
   `api.Generate` does not auto-inject it, unlike the CLI).

2. Corresponding Go structs in `pbgql`:
   ```go
   type SearchRequestQuery struct {
       Text   *string `json:"text"`
       Author *string `json:"author"`
   }
   type SearchRequestInput struct {
       Query *SearchRequestQuery `json:"query"`
   }
   ```
   Fields on a `@oneOf` input are nullable; gqlgen populates exactly one.

3. In `gqlgen.yml`, the input GraphQL types are bound to the pbgql structs (NOT
   to the pb types):
   ```yaml
   SearchRequest:      { model: .../pbgql.SearchRequestInput }
   SearchRequestQuery: { model: .../pbgql.SearchRequestQuery }
   ```

4. gqlgen emits the resolver signature with the pbgql input type:
   ```go
   SearchBooks(ctx context.Context, input pbgql.SearchRequestInput) (*pb.SearchResponse, error)
   ```

5. The resolver converts via a small `ToPb*` shim emitted by the generator:
   ```go
   func ToPbSearchRequest(r *SearchRequestInput) *pb.SearchRequest {
       req := &pb.SearchRequest{}
       if r.Query != nil {
           switch {
           case r.Query.Text != nil:
               req.Query = &pb.SearchRequest_Text{Text: *r.Query.Text}
           case r.Query.Author != nil:
               req.Query = &pb.SearchRequest_Author{Author: *r.Query.Author}
           }
       }
       return req
   }
   ```
   The resolver calls: `r.Library.SearchBooks(ctx, pbgql.ToPbSearchRequest(&input))`.

This `ToPb*` shim is **the only conversion code** in the entire generated output.
All non-oneof messages, enums, and scalars bind directly to pb types.

---

## The golden example: `SearchBooks`

`example/golden.proto` demonstrates both patterns in one service:

```proto
// Input oneof: caller provides text OR author
message SearchRequest {
  oneof query {
    string text   = 1;
    string author = 2;
  }
}

// Output oneof: result is a Book OR a NotFound
message SearchResponse {
  oneof result {
    Book     book      = 1;
    NotFound not_found = 2;
  }
}

service Library {
  rpc SearchBooks(SearchRequest) returns (SearchResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;  // → Query
  }
}
```

The generated GraphQL query (from `example/gen/gqlapi/schema.graphql`):

```graphql
union SearchResponseResult = SearchResponseResultBook | SearchResponseResultNotFound

input SearchRequestQuery @oneOf { text: String; author: String }
input SearchRequest { query: SearchRequestQuery }

type Query {
  searchBooks(input: SearchRequest!): SearchResponse!
}
```

Example client query selecting the union discriminant:

```graphql
{
  searchBooks(input: { query: { text: "Dune" } }) {
    result {
      __typename
      ... on SearchResponseResultBook     { id title }
      ... on SearchResponseResultNotFound { reason }
    }
  }
}
```

The runtime tests in `example/server_test.go` verify all four paths:
`TestSearchBooks_OutputUnion_Book`, `TestSearchBooks_OutputUnion_NotFound`,
`TestSearchBooks_InputOneof_Text`, and `TestSearchBooks_InputOneof_Author`.
