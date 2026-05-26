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
