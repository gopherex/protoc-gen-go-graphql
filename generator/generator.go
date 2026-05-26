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
	for _, svc := range f.Services {
		for _, m := range svc.Methods {
			if err := checkStreaming(
				svc.GoName,
				m.GoName,
				m.Desc.IsStreamingClient(),
				m.Desc.IsStreamingServer(),
			); err != nil {
				return err
			}
		}
	}
	return nil
}
