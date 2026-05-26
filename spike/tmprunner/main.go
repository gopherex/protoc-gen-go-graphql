// Spike runner: prove gqlgen can produce exec.go from a synthetic module
// (pb + pbgql only) with SkipValidation/SkipModTidy, run with CWD = module root.
package main

import (
	"fmt"
	"os"

	"github.com/99designs/gqlgen/api"
	"github.com/99designs/gqlgen/codegen/config"
)

func main() {
	cfg, err := config.LoadConfig(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(2)
	}
	cfg.SkipValidation = true // skip post-gen compile-check of exec/resolver
	cfg.SkipModTidy = true    // no post-gen `go mod tidy` (no network)
	if err := api.Generate(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "generate:", err)
		os.Exit(3)
	}
	fmt.Println("OK: exec generated")
}
