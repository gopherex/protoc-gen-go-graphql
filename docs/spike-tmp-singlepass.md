# Spike: /tmp single-pass (proof)

Goal: avoid the two-phase build (`go generate` for gqlgen) by having the plugin
itself run gqlgen against a synthetic module in /tmp, in one protoc/easyp run.

## Proven

gqlgen ran in a synthetic module dir containing ONLY pb + pbgql + schema.graphql +
gqlgen.yml (NO resolver.go), and produced a compiling `exec.go`:
- `cfg.SkipValidation = true` — skips the post-gen compile-check, so the resolver
  need NOT exist and exec need not link.
- `cfg.SkipModTidy = true` — no post-gen `go mod tidy` (no network).
- CWD = the gqlgen.yml directory (exec.filename resolves relative to CWD). go/packages
  resolves the module from any subdir (walks up to go.mod).
- Time: ~0.36s warm build cache. Cold: slower once (type-checks protobuf/grpc).
- `go build ./...` of the mirror (pb + pbgql + generated exec, no resolver) → exit 0.

Spike runner: `spike/tmprunner/main.go`. Mirror was built from `example/gen`
(pb + pbgql + schema + gqlgen.yml) + the module go.mod/go.sum.

## Remaining work for a real single-pass plugin
1. In the plugin: generate pb from the received FileDescriptorSet (shell
   protoc-gen-go) into /tmp; write pbgql + schema + gqlgen.yml to /tmp.
2. Provide a buildable /tmp module: copy the USER's go.mod/go.sum (found via CWD
   walk-up) — their module already requires gqlgen+graphqlpb+grpc+protobuf to
   compile the output. (First-run onboarding: user must `go get` those deps.)
3. Run gqlgen in-process with CWD = the /tmp config dir, SkipValidation+SkipModTidy.
4. Read exec.go + models_gen.go back; emit via CodeGeneratorResponse to the real tree.
5. Cleanup /tmp.

## Main residual risk
- The /tmp module's deps must resolve from cache (copy user's go.mod/go.sum). On a
  user's very first generation (before they depend on gqlgen/graphqlpb) the load
  fails until they `go get` the deps. Chicken-and-egg onboarding step.
- Shelling protoc-gen-go from inside our plugin + plumbing exec back.
- Cold build cache → slower first run.
