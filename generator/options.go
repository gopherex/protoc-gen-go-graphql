package generator

import (
	"fmt"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	descriptorpb "google.golang.org/protobuf/types/descriptorpb"

	"github.com/gopherex/protoc-gen-go-graphql/graphqlopt"
)

// The following helpers read the graphqlopt.* custom options off a descriptor.
// Each returns nil when the option is unset. Importing graphqlopt above also
// registers the extensions so proto.GetExtension can resolve them.

func fileOpts(f *protogen.File) *graphqlopt.FileOptions {
	opts, ok := f.Desc.Options().(*descriptorpb.FileOptions)
	if !ok || opts == nil || !proto.HasExtension(opts, graphqlopt.E_File) {
		return nil
	}
	return proto.GetExtension(opts, graphqlopt.E_File).(*graphqlopt.FileOptions)
}

func serviceOpts(s *protogen.Service) *graphqlopt.ServiceOptions {
	opts, ok := s.Desc.Options().(*descriptorpb.ServiceOptions)
	if !ok || opts == nil || !proto.HasExtension(opts, graphqlopt.E_Service) {
		return nil
	}
	return proto.GetExtension(opts, graphqlopt.E_Service).(*graphqlopt.ServiceOptions)
}

func methodOpts(m *protogen.Method) *graphqlopt.MethodOptions {
	opts, ok := m.Desc.Options().(*descriptorpb.MethodOptions)
	if !ok || opts == nil || !proto.HasExtension(opts, graphqlopt.E_Method) {
		return nil
	}
	return proto.GetExtension(opts, graphqlopt.E_Method).(*graphqlopt.MethodOptions)
}

func messageOpts(m *protogen.Message) *graphqlopt.MessageOptions {
	opts, ok := m.Desc.Options().(*descriptorpb.MessageOptions)
	if !ok || opts == nil || !proto.HasExtension(opts, graphqlopt.E_Message) {
		return nil
	}
	return proto.GetExtension(opts, graphqlopt.E_Message).(*graphqlopt.MessageOptions)
}

func fieldOpts(f *protogen.Field) *graphqlopt.FieldOptions {
	opts, ok := f.Desc.Options().(*descriptorpb.FieldOptions)
	if !ok || opts == nil || !proto.HasExtension(opts, graphqlopt.E_Field) {
		return nil
	}
	return proto.GetExtension(opts, graphqlopt.E_Field).(*graphqlopt.FieldOptions)
}

func enumOpts(e *protogen.Enum) *graphqlopt.EnumOptions {
	opts, ok := e.Desc.Options().(*descriptorpb.EnumOptions)
	if !ok || opts == nil || !proto.HasExtension(opts, graphqlopt.E_Enum) {
		return nil
	}
	return proto.GetExtension(opts, graphqlopt.E_Enum).(*graphqlopt.EnumOptions)
}

func oneofOpts(o *protogen.Oneof) *graphqlopt.OneofOptions {
	opts, ok := o.Desc.Options().(*descriptorpb.OneofOptions)
	if !ok || opts == nil || !proto.HasExtension(opts, graphqlopt.E_Oneof) {
		return nil
	}
	return proto.GetExtension(opts, graphqlopt.E_Oneof).(*graphqlopt.OneofOptions)
}

// methodSkipped reports whether the method is marked skip.
func methodSkipped(m *protogen.Method) bool {
	o := methodOpts(m)
	return o != nil && o.GetSkip()
}

// serviceSkipped reports whether the service is marked skip.
func serviceSkipped(s *protogen.Service) bool {
	o := serviceOpts(s)
	return o != nil && o.GetSkip()
}

// messageSkipped reports whether the message is marked skip.
func messageSkipped(m *protogen.Message) bool {
	if m == nil {
		return false
	}
	o := messageOpts(m)
	return o != nil && o.GetSkip()
}

// validateSkippedReferences fails fast when a non-skipped rpc (in a non-skipped
// service) references — via its request or response, transitively — a message
// that is marked skip. Such a reference cannot be satisfied because the skipped
// message is absent from the generated schema.
func validateSkippedReferences(files []*protogen.File) error {
	var walk func(m *protogen.Message, seen map[string]bool, rpc string) error
	walk = func(m *protogen.Message, seen map[string]bool, rpc string) error {
		if m == nil || m.Desc.IsMapEntry() {
			return nil
		}
		if messageSkipped(m) {
			return fmt.Errorf("message %s is skipped but referenced by rpc %s", m.GoIdent.GoName, rpc)
		}
		key := string(m.Desc.FullName())
		if seen[key] {
			return nil
		}
		seen[key] = true
		for _, field := range m.Fields {
			if field.Desc.IsMap() {
				continue
			}
			switch field.Desc.Kind() {
			case protoreflect.MessageKind, protoreflect.GroupKind:
				fqn := string(field.Desc.Message().FullName())
				if _, wkt := wellKnownGQLType[fqn]; wkt {
					continue
				}
				if err := walk(field.Message, seen, rpc); err != nil {
					return err
				}
			}
		}
		return nil
	}

	for _, f := range files {
		for _, svc := range f.Services {
			if serviceSkipped(svc) {
				continue
			}
			for _, m := range svc.Methods {
				if methodSkipped(m) {
					continue
				}
				rpc := svc.GoName + "." + m.GoName
				seen := map[string]bool{}
				if err := walk(m.Input, seen, rpc); err != nil {
					return err
				}
				if err := walk(m.Output, seen, rpc); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// validateOperationOverrides fails fast on operation overrides that conflict
// with the rpc's streaming shape: a server-streaming rpc forced to QUERY or
// MUTATION, or a unary rpc forced to SUBSCRIPTION (subscriptions require a
// stream source).
func validateOperationOverrides(g *graph) error {
	for _, svc := range g.Services {
		for _, m := range includedMethods(svc) {
			o := methodOpts(m)
			if o == nil {
				continue
			}
			rpc := svc.GoName + "." + m.GoName
			switch o.GetOperation() {
			case graphqlopt.Operation_QUERY, graphqlopt.Operation_MUTATION:
				if m.Desc.IsStreamingServer() {
					return fmt.Errorf("graphqlopt: rpc %s is server-streaming and cannot be forced to %s; server-streaming maps to SUBSCRIPTION", rpc, o.GetOperation())
				}
			case graphqlopt.Operation_SUBSCRIPTION:
				if !m.Desc.IsStreamingServer() {
					return fmt.Errorf("graphqlopt: rpc %s is unary and cannot be forced to SUBSCRIPTION; subscriptions require a server-streaming rpc", rpc)
				}
			}
		}
	}
	return nil
}

// validateUnsupportedOptions fails fast when an option that is part of the
// graphqlopt surface but not yet wired is set to a non-zero value. These are
// implemented in a later chunk; silently ignoring them would surprise users.
func validateUnsupportedOptions(g *graph) error {
	notImpl := func(name string) error {
		return fmt.Errorf("graphqlopt: %s is not yet implemented", name)
	}

	for _, f := range g.Files {
		if o := fileOpts(f); o != nil {
			if o.GetPbPackage() != "" {
				return notImpl("FileOptions.pb_package")
			}
			if o.GetSchemaFilename() != "" {
				return notImpl("FileOptions.schema_filename")
			}
			if o.GetGqlgenConfigFilename() != "" {
				return notImpl("FileOptions.gqlgen_config_filename")
			}
			if o.GetExecPackage() != "" {
				return notImpl("FileOptions.exec_package")
			}
			if o.GetExecFilename() != "" {
				return notImpl("FileOptions.exec_filename")
			}
		}
	}

	for _, svc := range g.Services {
		if o := serviceOpts(svc); o != nil {
			if o.GetNamePrefix() != "" {
				return notImpl("ServiceOptions.name_prefix")
			}
		}
	}

	for _, msg := range g.Messages {
		if o := messageOpts(msg); o != nil {
			if o.GetName() != "" {
				return notImpl("MessageOptions.name")
			}
		}
		for _, field := range msg.Fields {
			if o := fieldOpts(field); o != nil {
				if o.GetName() != "" {
					return notImpl("FieldOptions.name")
				}
				if o.GetExclude() {
					return notImpl("FieldOptions.exclude")
				}
				if o.GetScalar() != "" {
					return notImpl("FieldOptions.scalar")
				}
			}
		}
		for _, oo := range msg.Oneofs {
			if o := oneofOpts(oo); o != nil {
				if o.GetUnionName() != "" {
					return notImpl("OneofOptions.union_name")
				}
				if o.GetInputMode() != graphqlopt.OneofInputMode_ONEOF_INPUT_UNSPECIFIED {
					return notImpl("OneofOptions.input_mode")
				}
			}
		}
	}

	for _, e := range g.Enums {
		if o := enumOpts(e); o != nil {
			if o.GetName() != "" {
				return notImpl("EnumOptions.name")
			}
		}
	}

	return nil
}
