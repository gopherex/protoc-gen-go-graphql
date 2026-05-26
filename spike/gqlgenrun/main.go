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
