package generator

import (
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// graph is the GraphQL generation unit. A graph may contain several proto files
// from the same Go package plus message/enum types reachable through RPC
// requests and responses in imported packages.
type graph struct {
	Files    []*protogen.File
	Messages []*protogen.Message
	Enums    []*protogen.Enum
	Services []*protogen.Service
}

func messageKey(msg *protogen.Message) string {
	if msg == nil {
		return ""
	}
	return string(msg.GoIdent.GoImportPath) + "." + msg.GoIdent.GoName
}

func graphFromFile(f *protogen.File) *graph {
	return graphFromFiles([]*protogen.File{f})
}

func graphFromFiles(files []*protogen.File) *graph {
	g := &graph{Files: files}
	seenMsgs := map[string]bool{}
	seenEnums := map[string]bool{}

	addEnum := func(e *protogen.Enum) {
		key := string(e.Desc.FullName())
		if seenEnums[key] {
			return
		}
		seenEnums[key] = true
		g.Enums = append(g.Enums, e)
	}

	var addMessage func(*protogen.Message)
	addMessage = func(m *protogen.Message) {
		if m == nil || m.Desc.IsMapEntry() {
			return
		}
		// Skipped messages are entirely omitted from the graph and not
		// traversed into. A non-skipped rpc that references a skipped message
		// is caught as a fail-fast error in validateSkippedReferences.
		if messageSkipped(m) {
			return
		}
		key := string(m.Desc.FullName())
		if seenMsgs[key] {
			return
		}
		seenMsgs[key] = true
		g.Messages = append(g.Messages, m)
		for _, e := range m.Enums {
			addEnum(e)
		}
		for _, child := range m.Messages {
			addMessage(child)
		}
		for _, field := range m.Fields {
			if field.Desc.IsMap() {
				if field.Desc.MapKey().Kind() == protoreflect.EnumKind && field.Enum != nil {
					addEnum(field.Enum)
				}
				continue
			}
			switch field.Desc.Kind() {
			case protoreflect.MessageKind, protoreflect.GroupKind:
				fqn := string(field.Desc.Message().FullName())
				if _, wkt := wellKnownGQLType[fqn]; !wkt {
					addMessage(field.Message)
				}
			case protoreflect.EnumKind:
				addEnum(field.Enum)
			}
		}
	}

	for _, f := range files {
		for _, e := range f.Enums {
			addEnum(e)
		}
		for _, m := range f.Messages {
			if messageSkipped(m) {
				continue
			}
			addMessage(m)
		}
	}
	for _, f := range files {
		for _, svc := range f.Services {
			if serviceSkipped(svc) {
				continue
			}
			g.Services = append(g.Services, svc)
			for _, m := range svc.Methods {
				if methodSkipped(m) {
					continue
				}
				addMessage(m.Input)
				addMessage(m.Output)
			}
		}
	}

	return g
}

// graphHasOperations reports whether the graph has at least one non-skipped
// service method, i.e. at least one GraphQL Query/Mutation/Subscription root field.
func graphHasOperations(g *graph) bool {
	for _, svc := range g.Services {
		if len(includedMethods(svc)) > 0 {
			return true
		}
	}
	return false
}

// includedMethods returns the methods of a service that are not skipped.
func includedMethods(svc *protogen.Service) []*protogen.Method {
	var out []*protogen.Method
	for _, m := range svc.Methods {
		if methodSkipped(m) {
			continue
		}
		out = append(out, m)
	}
	return out
}
