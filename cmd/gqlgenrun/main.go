// Command gqlgenrun is the in-process gqlgen runner invoked by //go:generate in
// the emitted gqlapi/generate.go. It must live in its own directory (not the
// resolver package dir) because gqlgen's package loader reads all files in the
// resolver directory and a package main there conflicts with the resolver package
// name (spike-findings.md §6).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/99designs/gqlgen/api"
	"github.com/99designs/gqlgen/codegen/config"
)

func main() {
	cfg_path := flag.String("config", "gqlgen.yml", "path to gqlgen.yml")
	flag.Parse()

	cfg, err := config.LoadConfig(*cfg_path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(2)
	}
	if err := api.Generate(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
}
