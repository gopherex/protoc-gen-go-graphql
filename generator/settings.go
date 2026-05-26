package generator

import "flag"

const (
	defaultOutDir    = "gqlapi"
	defaultRunnerPkg = "github.com/gopherex/protoc-gen-go-graphql/cmd/gqlgenrun"
)

type Settings struct {
	Paths      string // source_relative etc., passed through to protoc convention
	OutDir     string // subpackage directory + Go package name for generated GraphQL code (default: "gqlapi")
	RunnerPkg  string // go:generate runner import path (default: our cmd/gqlgenrun)
	SinglePass bool   // run gqlgen inside the plugin (no separate go generate step)
}

func (s *Settings) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&s.Paths, "paths", "", "paths mode (source_relative)")
	fs.StringVar(&s.OutDir, "out_dir", defaultOutDir, "subpackage dir/package name for GraphQL output (default: gqlapi)")
	fs.StringVar(&s.RunnerPkg, "runner", defaultRunnerPkg, "go:generate runner import path (default: github.com/gopherex/protoc-gen-go-graphql/cmd/gqlgenrun)")
	fs.BoolVar(&s.SinglePass, "single_pass", false, "run gqlgen inside the plugin (no separate go generate step)")
}

// applyDefaults fills in empty flag values with their defaults.
// Called by the generator before use so that zero-value Settings still work.
func (s *Settings) applyDefaults() {
	if s.OutDir == "" {
		s.OutDir = defaultOutDir
	}
	if s.RunnerPkg == "" {
		s.RunnerPkg = defaultRunnerPkg
	}
}
