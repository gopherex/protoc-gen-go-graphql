package generator

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// buildResolvers emits the resolver.go source for the gqlapi package.
// Parameters:
//   - f: the proto file descriptor
//   - pkgName: Go package name for the generated file (e.g. "gqlapi")
//   - pbImport: Go import path of the pb package (e.g. "...example/gen")
//   - pbgqlImport: Go import path of the pbgql package (e.g. "...example/gen/gqlapi/pbgql")
//   - execImport: Go import path of the exec package (e.g. "...example/gen/gqlapi/exec")
//   - runtimeImport: Go import path of the runtime package
func buildResolvers(f *protogen.File, pkgName, pbImport, pbgqlImport, execImport, runtimeImport string) string {
	return buildResolversGraph(graphFromFile(f), pkgName, pbImport, pbgqlImport, execImport, runtimeImport)
}

func buildResolversGraph(g *graph, pkgName, pbImport, pbgqlImport, execImport, runtimeImport string) string {
	var body strings.Builder

	// Collect oneof info.
	msgInfo := analyzeMessagesGraph(g)
	ois := collectOneofsGraph(g, msgInfo)

	// Index by messageKey.
	outputOneofsByMsg := map[string][]oneofInfo{} // messageKey(msg) → output oneofs
	inputOneofsByMsg := map[string]oneofInfo{}    // messageKey(reqMsg) → input oneof (at most one per request)
	for _, oi := range ois {
		key := messageKey(oi.Msg)
		if oi.IsOutput {
			outputOneofsByMsg[key] = append(outputOneofsByMsg[key], oi)
		}
		if oi.IsInput {
			inputOneofsByMsg[key] = oi
		}
	}

	pbAliases := map[string]string{pbImport: "pb"}
	var pbImports []string
	addPbImport := func(importPath protogen.GoImportPath) {
		path := string(importPath)
		if path == "" || path == pbImport {
			return
		}
		if _, ok := pbAliases[path]; ok {
			return
		}
		pbAliases[path] = fmt.Sprintf("pb%d", len(pbAliases))
		pbImports = append(pbImports, path)
	}
	for _, svc := range g.Services {
		for _, m := range includedMethods(svc) {
			addPbImport(m.Input.GoIdent.GoImportPath)
			addPbImport(m.Output.GoIdent.GoImportPath)
		}
	}
	for _, msg := range g.Messages {
		addPbImport(msg.GoIdent.GoImportPath)
	}
	pbType := func(id protogen.GoIdent) string {
		path := string(id.GoImportPath)
		alias := pbAliases[path]
		if alias == "" {
			alias = "pb"
		}
		return alias + "." + id.GoName
	}

	// Determine if pbgql import is needed (any oneof present).
	needsPbgql := len(ois) > 0

	// Determine if WKT JSON field resolvers are needed (any WKT JSON fields in output types).
	needsWKTJSON := false
	for _, msg := range g.Messages {
		mi := msgInfo[messageKey(msg)]
		if mi == nil || !mi.role.has(roleOutput) {
			continue
		}
		for _, field := range msg.Fields {
			if field.Desc.Kind() == protoreflect.MessageKind {
				fqn := string(field.Desc.Message().FullName())
				if wktJSONTypes[fqn] {
					needsWKTJSON = true
					break
				}
			}
		}
		if needsWKTJSON {
			break
		}
	}

	// The body (Resolver struct + sub-resolvers + methods) is built first, into the
	// `body` builder, so the import block can omit extra pb package aliases that no
	// resolver method actually references (an imported-and-not-used package is a
	// compile error). The primary `pb` import is always used (Resolver struct).

	// Resolver struct: one field per service (svcName pb.SvcServer).
	body.WriteString("// Resolver delegates GraphQL operations to the gRPC server implementation.\n")
	body.WriteString("// It implements exec.ResolverRoot and each sub-resolver interface,\n")
	body.WriteString("// delegating to the gRPC server with zero proto<->model conversion.\n")
	body.WriteString("type Resolver struct {\n")
	for _, svc := range g.Services {
		fmt.Fprintf(&body, "\t%s pb.%sServer\n", svc.GoName, svc.GoName)
	}
	body.WriteString("}\n")
	body.WriteString("\n")

	// Collect info needed for sub-resolvers.
	// Map: resolver type name → methods
	type resolverMethod struct {
		receiverType string
		goName       string
		inputType    string
		outputType   string
		opType       string
		streamType   string // for subscriptions
		svcName      string
	}

	// Collect map-field resolvers (for output types with map fields).
	type mapFieldResolver struct {
		ownerType     string // e.g. "Book"
		ownerPbType   string // Go type of the receiver, e.g. "*pb.Book" or "*pbgql.SearchResponseResultBook"
		ownerPkg      string // "pb" or "pbgql"
		fieldName     string // camelCase, e.g. "tags"
		mapGetter     string // e.g. "GetTags" (relative to embedded pb type)
		embeddedField string // for wrappers: the embedded pb msg name; empty for direct pb types
	}
	var mapResolvers []mapFieldResolver

	// Collect empty-output-message resolvers (for fieldless messages that get an "ok" placeholder).
	type emptyOutputResolver struct {
		ownerType   string // e.g. "PingResponse"
		ownerPbType string // e.g. "*pb.PingResponse"
	}
	var emptyOutputResolvers []emptyOutputResolver

	// Collect empty-input-message resolvers. A fieldless message used as a nested
	// GraphQL input gets a placeholder field (`_empty`) in the schema, emitted with
	// @goField(forceResolver:true). gqlgen generates an input resolver interface
	// <TypeName>InputResolver with an Empty(ctx, obj, data) method; we implement it
	// as a no-op (the empty pb message needs nothing set).
	type emptyInputResolver struct {
		gqlInputType string // e.g. "Container_SettingsInput" (the GraphQL input type name)
		ownerPbType  string // e.g. "*pb.Container_Settings"
	}
	var emptyInputResolvers []emptyInputResolver

	// oneofsByMsg: all oneofs keyed by message, needed by inputFields below.
	oneofsByMsg := map[string][]oneofInfo{}
	for _, oi := range ois {
		oneofsByMsg[messageKey(oi.Msg)] = append(oneofsByMsg[messageKey(oi.Msg)], oi)
	}
	for _, msg := range g.Messages {
		mi := msgInfo[messageKey(msg)]
		// Nested (non-request) input messages that produce a fieldless input block.
		if mi == nil || !mi.role.has(roleInput) || mi.isRequest {
			continue
		}
		if len(inputFields(msg, msgInfo, oneofsByMsg)) != 0 {
			continue
		}
		emptyInputResolvers = append(emptyInputResolvers, emptyInputResolver{
			gqlInputType: gqlTypeName(msg) + "Input",
			ownerPbType:  "*" + pbType(msg.GoIdent),
		})
	}

	// Collect incompatible-type field resolvers (float32 → float64, uint32 → int, WKT→JSON).
	// These fields are emitted with @goField(forceResolver:true) in the schema and
	// need resolver methods that coerce to the gqlgen-expected Go type.
	type coerceFieldResolver struct {
		ownerType   string // e.g. "ScalarTypes"
		ownerPbType string // e.g. "*pb.ScalarTypes"
		fieldName   string // camelCase GraphQL field name
		getter      string // pb getter name, e.g. "GetFieldFloat"
		pbField     string // pb struct field Go name, e.g. "FieldFloat" (for optional nil-checks)
		retType     string // return type for resolver method, e.g. "float64" or "int"
		isOptional  bool   // true when field is optional (returns pointer)
		isList      bool   // true when field is repeated (returns slice)
		pbElemType  string // Go proto element type for repeated fields, e.g. "float32"
		isWKTJSON   bool   // true for WKT types that map to JSON scalar (uses protojson)
		wktPbType   string // Go type of the WKT field, e.g. "*anypb.Any"
	}
	var coerceResolvers []coerceFieldResolver

	// First: direct pb message map fields (including nested messages) and empty output resolvers.
	for _, msg := range g.Messages {
		mi := msgInfo[messageKey(msg)]
		// Only generate resolvers for types that are reachable as output.
		if mi == nil || !mi.role.has(roleOutput) {
			continue
		}
		// Empty output message: needs an "ok" placeholder field resolver.
		if isEmptyMessage(msg) {
			emptyOutputResolvers = append(emptyOutputResolvers, emptyOutputResolver{
				ownerType:   msg.GoIdent.GoName,
				ownerPbType: "*" + pbType(msg.GoIdent),
			})
			continue
		}
		for _, field := range msg.Fields {
			if fieldExcluded(field) {
				continue
			}
			if field.Desc.IsMap() {
				camel := gqlFieldName(field)
				getter := "Get" + string(field.GoName)
				mapResolvers = append(mapResolvers, mapFieldResolver{
					ownerType:   msg.GoIdent.GoName,
					ownerPbType: "*" + pbType(msg.GoIdent),
					ownerPkg:    "pb",
					fieldName:   camel,
					mapGetter:   getter,
				})
			} else if needsForceResolver(field) {
				camel := gqlFieldName(field)
				getter := "Get" + string(field.GoName)
				isWKTJSON := false
				wktPb := ""
				if field.Desc.Kind() == protoreflect.MessageKind {
					fqn := string(field.Desc.Message().FullName())
					if wktJSONTypes[fqn] {
						isWKTJSON = true
						wktPb = wktGoType(fqn)
					}
				}
				retType := coerceReturnType(field)
				pbElem := pbElemType(field)
				coerceResolvers = append(coerceResolvers, coerceFieldResolver{
					ownerType:   msg.GoIdent.GoName,
					ownerPbType: "*" + pbType(msg.GoIdent),
					fieldName:   camel,
					getter:      getter,
					pbField:     string(field.GoName),
					retType:     retType,
					isOptional:  field.Desc.HasOptionalKeyword(),
					isList:      field.Desc.IsList(),
					pbElemType:  pbElem,
					isWKTJSON:   isWKTJSON,
					wktPbType:   wktPb,
				})
			}
		}
	}

	// Second: union member wrapper types that embed a pb message with map fields.
	// For each output oneof's variants that wrap a message with map fields, we need
	// a field resolver on the wrapper type too.
	for _, oi := range ois {
		if !oi.IsOutput {
			continue
		}
		for _, v := range oi.Variants {
			if !v.IsMessage || v.Msg == nil {
				continue
			}
			for _, field := range v.Msg.Fields {
				if fieldExcluded(field) {
					continue
				}
				if field.Desc.IsMap() {
					camel := gqlFieldName(field)
					getter := "Get" + string(field.GoName)
					// The receiver is the wrapper type (in pbgql), not the pb type.
					mapResolvers = append(mapResolvers, mapFieldResolver{
						ownerType:     v.WrapperGoName,
						ownerPbType:   "*pbgql." + v.WrapperGoName,
						ownerPkg:      "pbgql",
						fieldName:     camel,
						mapGetter:     getter,
						embeddedField: "", // Go finds GetTags via embedding
					})
				} else if needsForceResolver(field) {
					camel := gqlFieldName(field)
					getter := "Get" + string(field.GoName)
					isWKTJSON := false
					wktPb := ""
					if field.Desc.Kind() == protoreflect.MessageKind {
						fqn := string(field.Desc.Message().FullName())
						if wktJSONTypes[fqn] {
							isWKTJSON = true
							wktPb = wktGoType(fqn)
						}
					}
					retType := coerceReturnType(field)
					pbElem := pbElemType(field)
					coerceResolvers = append(coerceResolvers, coerceFieldResolver{
						ownerType:   v.WrapperGoName,
						ownerPbType: "*pbgql." + v.WrapperGoName,
						fieldName:   camel,
						getter:      getter,
						pbField:     string(field.GoName),
						retType:     retType,
						isOptional:  field.Desc.HasOptionalKeyword(),
						isList:      field.Desc.IsList(),
						pbElemType:  pbElem,
						isWKTJSON:   isWKTJSON,
						wktPbType:   wktPb,
					})
				}
			}
		}
	}

	// Determine which output types need a sub-resolver:
	//   - types with map fields (for JSON scalar field resolvers)
	//   - types with output oneofs (for union field resolvers)
	//   - types with incompatible fields (float32, uint32)
	//   - empty output types (for the placeholder "ok" field resolver)
	resolverTypes := map[string]bool{} // e.g. "Book" → true
	for _, mr := range mapResolvers {
		resolverTypes[mr.ownerType] = true
	}
	for _, cr := range coerceResolvers {
		resolverTypes[cr.ownerType] = true
	}
	for _, oi := range ois {
		if oi.IsOutput {
			resolverTypes[oi.MsgGoName] = true
		}
	}
	for _, er := range emptyOutputResolvers {
		resolverTypes[er.ownerType] = true
	}
	for _, er := range emptyInputResolvers {
		resolverTypes[er.gqlInputType] = true
	}

	// Operation sub-resolver types: queryResolver, mutationResolver, etc.
	opTypes := map[string]bool{}
	for _, svc := range g.Services {
		for _, m := range includedMethods(svc) {
			op := operationType(m)
			opTypes[strings.ToLower(op)] = true
		}
	}

	// ResolverRoot interface methods.
	// For each output type that has map/oneof fields → <TypeName>() exec.<TypeName>Resolver
	// For each op type → <OpType>() exec.<OpType>Resolver
	// Sort resolverTypes for deterministic output.
	resolverTypesSorted := sortedKeys(resolverTypes)
	for _, typeName := range resolverTypesSorted {
		recvName := strings.ToLower(typeName[:1]) + typeName[1:] + "Resolver"
		fmt.Fprintf(&body, "func (r *Resolver) %s() exec.%sResolver { return %s{r} }\n",
			typeName, typeName, recvName)
	}
	for _, opType := range []string{"Query", "Mutation", "Subscription"} {
		if opTypes[strings.ToLower(opType)] {
			recvName := strings.ToLower(opType) + "Resolver"
			fmt.Fprintf(&body, "func (r *Resolver) %s() exec.%sResolver { return %s{r} }\n",
				opType, opType, recvName)
		}
	}
	body.WriteString("\n")

	// Declare sub-resolver struct types.
	for _, typeName := range resolverTypesSorted {
		recvName := strings.ToLower(typeName[:1]) + typeName[1:] + "Resolver"
		fmt.Fprintf(&body, "type %s struct{ *Resolver }\n", recvName)
	}
	for _, opType := range []string{"Query", "Mutation", "Subscription"} {
		if opTypes[strings.ToLower(opType)] {
			recvName := strings.ToLower(opType) + "Resolver"
			fmt.Fprintf(&body, "type %s struct{ *Resolver }\n", recvName)
		}
	}
	body.WriteString("\n")

	// Map field resolver methods.
	for _, mr := range mapResolvers {
		recvName := strings.ToLower(mr.ownerType[:1]) + mr.ownerType[1:] + "Resolver"
		capField := goResolverName(mr.fieldName)
		fmt.Fprintf(&body, "// %s exposes the proto map field as a JSON scalar (field\n", capField)
		fmt.Fprintf(&body, "// resolver, because the JSON scalar's Go type `any` cannot bind a concrete map).\n")
		fmt.Fprintf(&body, "func (r %s) %s(ctx context.Context, obj %s) (any, error) {\n",
			recvName, capField, mr.ownerPbType)
		fmt.Fprintf(&body, "\treturn obj.%s(), nil\n", mr.mapGetter)
		body.WriteString("}\n")
		body.WriteString("\n")
	}

	// Incompatible-type field resolver methods (float32 → float64, uint32 → int).
	// These are needed because gqlgen cannot directly bind float32/uint32 to Float/Int.
	for _, cr := range coerceResolvers {
		recvName := strings.ToLower(cr.ownerType[:1]) + cr.ownerType[1:] + "Resolver"
		capField := goResolverName(cr.fieldName)
		fmt.Fprintf(&body, "// %s coerces the proto field to the gqlgen-compatible type.\n", capField)
		if cr.isWKTJSON && cr.isList {
			// Repeated WKT JSON: marshal each element via protojson into []any.
			fmt.Fprintf(&body, "func (r %s) %s(ctx context.Context, obj %s) ([]any, error) {\n",
				recvName, capField, cr.ownerPbType)
			fmt.Fprintf(&body, "\tsrc := obj.%s()\n", cr.getter)
			body.WriteString("\tout := make([]any, len(src))\n")
			body.WriteString("\tfor i, v := range src {\n")
			body.WriteString("\t\tif v == nil {\n\t\t\tcontinue\n\t\t}\n")
			body.WriteString("\t\tb, err := protojson.Marshal(v)\n")
			body.WriteString("\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n")
			body.WriteString("\t\tif err := json.Unmarshal(b, &out[i]); err != nil {\n\t\t\treturn nil, err\n\t\t}\n")
			body.WriteString("\t}\n")
			body.WriteString("\treturn out, nil\n")
		} else if cr.isWKTJSON {
			// WKT JSON field: marshal via protojson and decode into any.
			fmt.Fprintf(&body, "func (r %s) %s(ctx context.Context, obj %s) (any, error) {\n",
				recvName, capField, cr.ownerPbType)
			fmt.Fprintf(&body, "\tv := obj.%s()\n", cr.getter)
			body.WriteString("\tif v == nil {\n")
			body.WriteString("\t\treturn nil, nil\n")
			body.WriteString("\t}\n")
			body.WriteString("\tb, err := protojson.Marshal(v)\n")
			body.WriteString("\tif err != nil {\n")
			body.WriteString("\t\treturn nil, err\n")
			body.WriteString("\t}\n")
			body.WriteString("\tvar out any\n")
			body.WriteString("\tif err := json.Unmarshal(b, &out); err != nil {\n")
			body.WriteString("\t\treturn nil, err\n")
			body.WriteString("\t}\n")
			body.WriteString("\treturn out, nil\n")
		} else if cr.isList {
			// Repeated field: convert []pbType to []retType.
			fmt.Fprintf(&body, "func (r %s) %s(ctx context.Context, obj %s) ([]%s, error) {\n",
				recvName, capField, cr.ownerPbType, cr.retType)
			fmt.Fprintf(&body, "\tsrc := obj.%s()\n", cr.getter)
			fmt.Fprintf(&body, "\tout := make([]%s, len(src))\n", cr.retType)
			body.WriteString("\tfor i, v := range src {\n")
			fmt.Fprintf(&body, "\t\tout[i] = %s(v)\n", cr.retType)
			body.WriteString("\t}\n")
			body.WriteString("\treturn out, nil\n")
		} else if cr.isOptional {
			// Optional field: proto3 optional generates a *T field (nil when absent).
			// The resolver returns *RetType (nullable in GraphQL). The nil-check
			// accesses the pb struct field directly, so it must use the protoc-gen-go
			// field name (promoted through the wrapper for union-member resolvers).
			fmt.Fprintf(&body, "func (r %s) %s(ctx context.Context, obj %s) (*%s, error) {\n",
				recvName, capField, cr.ownerPbType, cr.retType)
			fmt.Fprintf(&body, "\tif obj.%s == nil {\n", cr.pbField)
			body.WriteString("\t\treturn nil, nil\n")
			body.WriteString("\t}\n")
			fmt.Fprintf(&body, "\tc := %s(obj.%s())\n", cr.retType, cr.getter)
			body.WriteString("\treturn &c, nil\n")
		} else {
			fmt.Fprintf(&body, "func (r %s) %s(ctx context.Context, obj %s) (%s, error) {\n",
				recvName, capField, cr.ownerPbType, cr.retType)
			fmt.Fprintf(&body, "\treturn %s(obj.%s()), nil\n", cr.retType, cr.getter)
		}
		body.WriteString("}\n")
		body.WriteString("\n")
	}

	// Empty output message "ok" placeholder field resolvers.
	for _, er := range emptyOutputResolvers {
		recvName := strings.ToLower(er.ownerType[:1]) + er.ownerType[1:] + "Resolver"
		fmt.Fprintf(&body, "// Ok is the placeholder field resolver for the empty message %s.\n", er.ownerType)
		fmt.Fprintf(&body, "func (r %s) Ok(ctx context.Context, obj %s) (bool, error) {\n",
			recvName, er.ownerPbType)
		body.WriteString("\treturn true, nil\n")
		body.WriteString("}\n")
		body.WriteString("\n")
	}

	// Empty input message placeholder resolvers. The `_empty` schema field maps to
	// a Go method named Empty; it is a no-op because the bound pb message is empty.
	for _, er := range emptyInputResolvers {
		recvName := strings.ToLower(er.gqlInputType[:1]) + er.gqlInputType[1:] + "Resolver"
		fmt.Fprintf(&body, "// Empty is the no-op placeholder resolver for the empty input %s.\n", er.gqlInputType)
		fmt.Fprintf(&body, "func (r %s) Empty(ctx context.Context, obj %s, data *bool) error {\n",
			recvName, er.ownerPbType)
		body.WriteString("\treturn nil\n")
		body.WriteString("}\n")
		body.WriteString("\n")
	}

	// Output oneof union field resolvers.
	for _, oi := range ois {
		if !oi.IsOutput {
			continue
		}
		recvName := strings.ToLower(oi.MsgGoName[:1]) + oi.MsgGoName[1:] + "Resolver"
		capField := goResolverName(oi.GQLFieldName)
		fmt.Fprintf(&body, "// %s resolves the oneof field %q as a %s union.\n",
			capField, oi.ProtoName, oi.UnionGQLName)
		fmt.Fprintf(&body, "// It wraps the pb oneof variant into the appropriate pbgql wrapper type.\n")
		fmt.Fprintf(&body, "func (r %s) %s(ctx context.Context, obj *%s) (pbgql.%s, error) {\n",
			recvName, capField, pbType(oi.Msg.GoIdent), oi.InterfaceGoName)
		fmt.Fprintf(&body, "\treturn pbgql.Wrap%s(obj), nil\n", oi.InterfaceGoName)
		body.WriteString("}\n")
		body.WriteString("\n")
	}

	// Operation resolver methods.
	for _, svc := range g.Services {
		for _, m := range includedMethods(svc) {
			op := operationType(m)
			recvName := strings.ToLower(op) + "Resolver"
			// methodName is the gqlgen-facing resolver method name (derived from
			// the GraphQL field name, honoring operation_name overrides).
			methodName := resolverMethodName(m)
			// rpcName is the gRPC client method to call (always the proto name).
			rpcName := m.GoName
			inputGoName := m.Input.GoIdent.GoName
			emptyReq := isEmptyMessage(m.Input)

			switch op {
			case "Query", "Mutation":
				if emptyReq {
					// Empty request: no input parameter; construct the empty request inline.
					fmt.Fprintf(&body, "func (r %s) %s(ctx context.Context) (*%s, error) {\n",
						recvName, methodName, pbType(m.Output.GoIdent))
					fmt.Fprintf(&body, "\tresp, err := r.%s.%s(ctx, &%s{})\n",
						svc.GoName, rpcName, pbType(m.Input.GoIdent))
				} else if inputOI, hasInputOneof := inputOneofsByMsg[messageKey(m.Input)]; hasInputOneof {
					// Input has a oneof: the resolver receives the intermediate pbgql
					// struct and converts it via ToPb (which may return an error,
					// e.g. ALL_NULLABLE mode's runtime exactly-one enforcement).
					fmt.Fprintf(&body, "func (r %s) %s(ctx context.Context, input pbgql.%s) (*%s, error) {\n",
						recvName, methodName, inputOI.MsgInputGoName, pbType(m.Output.GoIdent))
					fmt.Fprintf(&body, "\treq, err := pbgql.ToPb%s(&input)\n", inputGoName)
					body.WriteString("\tif err != nil {\n")
					body.WriteString("\t\treturn nil, graphqlpb.GraphQLError(ctx, err)\n")
					body.WriteString("\t}\n")
					fmt.Fprintf(&body, "\tresp, err := r.%s.%s(ctx, req)\n", svc.GoName, rpcName)
				} else {
					// Normal input: bind directly to pb.
					fmt.Fprintf(&body, "func (r %s) %s(ctx context.Context, input %s) (*%s, error) {\n",
						recvName, methodName, pbType(m.Input.GoIdent), pbType(m.Output.GoIdent))
					fmt.Fprintf(&body, "\tresp, err := r.%s.%s(ctx, &input)\n", svc.GoName, rpcName)
				}
				body.WriteString("\tif err != nil {\n")
				body.WriteString("\t\treturn nil, graphqlpb.GraphQLError(ctx, err)\n")
				body.WriteString("\t}\n")
				body.WriteString("\treturn resp, nil\n")
				body.WriteString("}\n")
				body.WriteString("\n")

			case "Subscription":
				// The streaming message type: for server-streaming the output IS the
				// message type (not a wrapper). For golden, WatchBooks returns stream Book,
				// so m.Output.GoIdent.GoName is "Book".
				if emptyReq {
					fmt.Fprintf(&body, "func (r %s) %s(ctx context.Context) (<-chan *%s, error) {\n",
						recvName, methodName, pbType(m.Output.GoIdent))
					fmt.Fprintf(&body, "\treturn graphqlpb.PumpServerStream[%s](ctx, func(ss *graphqlpb.StreamServer[%s]) error {\n",
						pbType(m.Output.GoIdent), pbType(m.Output.GoIdent))
					fmt.Fprintf(&body, "\t\treturn r.%s.%s(&%s{}, ss)\n", svc.GoName, rpcName, pbType(m.Input.GoIdent))
				} else {
					fmt.Fprintf(&body, "func (r %s) %s(ctx context.Context, input %s) (<-chan *%s, error) {\n",
						recvName, methodName, pbType(m.Input.GoIdent), pbType(m.Output.GoIdent))
					fmt.Fprintf(&body, "\treturn graphqlpb.PumpServerStream[%s](ctx, func(ss *graphqlpb.StreamServer[%s]) error {\n",
						pbType(m.Output.GoIdent), pbType(m.Output.GoIdent))
					fmt.Fprintf(&body, "\t\treturn r.%s.%s(&input, ss)\n", svc.GoName, rpcName)
				}
				body.WriteString("\t}), nil\n")
				body.WriteString("}\n")
				body.WriteString("\n")
			}
		}
	}

	bodyStr := body.String()

	// Header: package + imports. Extra pb aliases are emitted only when referenced
	// in the body, so a package that is imported by the type graph but never named
	// by a resolver method does not produce an "imported and not used" error.
	var sb strings.Builder
	sb.WriteString("// Code generated by protoc-gen-go-graphql. DO NOT EDIT.\n")
	sb.WriteString("package " + pkgName + "\n")
	sb.WriteString("\n")
	sb.WriteString("import (\n")
	sb.WriteString("\t\"context\"\n")
	if needsWKTJSON {
		sb.WriteString("\t\"encoding/json\"\n")
	}
	sb.WriteString("\n")
	// Third-party imports: alphabetical order within each group (gofmt canonical).
	fmt.Fprintf(&sb, "\tpb %q\n", pbImport)
	for _, importPath := range pbImports {
		alias := pbAliases[importPath]
		if !strings.Contains(bodyStr, alias+".") {
			continue // package not referenced by any resolver method
		}
		fmt.Fprintf(&sb, "\t%s %q\n", alias, importPath)
	}
	fmt.Fprintf(&sb, "\t%q\n", execImport)
	if needsPbgql {
		fmt.Fprintf(&sb, "\t%q\n", pbgqlImport)
	}
	fmt.Fprintf(&sb, "\t%q\n", runtimeImport)
	if needsWKTJSON {
		sb.WriteString("\t\"google.golang.org/protobuf/encoding/protojson\"\n")
	}
	sb.WriteString(")\n")
	sb.WriteString("\n")
	sb.WriteString(bodyStr)

	return sb.String()
}

// sortedKeys returns the keys of m in sorted order.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Sort deterministically.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] > keys[j] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}
