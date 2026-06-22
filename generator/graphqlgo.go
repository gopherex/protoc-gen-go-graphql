package generator

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// buildGraphQLGoGraph emits a single self-contained Go source file (package
// outPkg) that constructs a *graphql.Schema for the package's services. Every
// field gets an explicitly generated Resolve func that delegates to the user's
// pb.*ServiceServer implementations — there is no autobind and no second model
// set. This is the graphql-go backend (single protoc pass, no gqlgen).
func buildGraphQLGoGraph(g *graph, outPkg, pbImport, runtimeImport string) (string, error) {
	e := &gqlEmitter{
		g:           g,
		runtime:     "graphqlrt",
		msgInfo:     analyzeMessagesGraph(g),
		pbAliases:   map[string]string{pbImport: "pb"},
		pbImport:    pbImport,
		typeVars:    map[string]string{},
		scalarVar:   map[string]string{},
		wrapperBox:  map[string]scalarWrapperBox{},
		neededImps:  map[string]bool{},
	}
	e.ois = collectOneofsGraph(g, e.msgInfo)
	e.oneofsByMsg = map[string][]oneofInfo{}
	for _, oi := range e.ois {
		e.oneofsByMsg[messageKey(oi.Msg)] = append(e.oneofsByMsg[messageKey(oi.Msg)], oi)
	}
	return e.emit(outPkg, runtimeImport)
}

type scalarWrapperBox struct {
	goType   string // the Go struct name emitted for this scalar union variant wrapper
	valGo    string // Go type of the Value field, e.g. "string"
	objVar   string // graphql object var
	gqlName  string
}

type gqlEmitter struct {
	g          *graph
	runtime    string
	msgInfo    map[string]*messageInfo
	ois        []oneofInfo
	oneofsByMsg map[string][]oneofInfo
	pbImport   string
	pbAliases  map[string]string
	extraImps  []string
	typeVars   map[string]string // gqlName → object/enum/union var
	scalarVar  map[string]string // input object var by gqlName
	wrapperBox map[string]scalarWrapperBox // WrapperGoName → box
	neededImps map[string]bool
	declaredVars []string // all declared graphql type vars (for cfg.Types registration)
}

// decl writes a `var <v> <typ>` declaration and records the var for cfg.Types, so
// every declared type is referenced (no "declared and not used") and registered.
func (e *gqlEmitter) decl(decls *strings.Builder, v, typ string) {
	decls.WriteString("\tvar " + v + " " + typ + "\n")
	e.declaredVars = append(e.declaredVars, v)
}

func (e *gqlEmitter) addImport(p protogen.GoImportPath) {
	path := string(p)
	if path == "" || path == e.pbImport {
		return
	}
	if _, ok := e.pbAliases[path]; ok {
		return
	}
	e.pbAliases[path] = fmt.Sprintf("pb%d", len(e.pbAliases))
	e.extraImps = append(e.extraImps, path)
}

func (e *gqlEmitter) qual(id protogen.GoIdent) string {
	e.addImport(id.GoImportPath)
	alias := e.pbAliases[string(id.GoImportPath)]
	if alias == "" {
		alias = "pb"
	}
	return alias + "." + id.GoName
}

// objVar/enumVar/unionVar/inputVar return deterministic Go var names.
func objVar(gql string) string    { return "o_" + sanitizeIdent(gql) }
func enumVar(gql string) string   { return "e_" + sanitizeIdent(gql) }
func unionVar(gql string) string  { return "u_" + sanitizeIdent(gql) }
func inputVar(gql string) string  { return "i_" + sanitizeIdent(gql) }

func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// scalarTypeExpr maps a GraphQL scalar name to a graphql-go Type expression.
func (e *gqlEmitter) scalarTypeExpr(name string) string {
	switch name {
	case "String":
		return "graphql.String"
	case "Boolean":
		return "graphql.Boolean"
	case "Int":
		return "graphql.Int"
	case "Float":
		return "graphql.Float"
	case "Int64":
		return e.runtime + ".Int64"
	case "Uint64":
		return e.runtime + ".Uint64"
	case "Bytes":
		return e.runtime + ".Bytes"
	case "Timestamp":
		return e.runtime + ".Timestamp"
	case "Duration":
		return e.runtime + ".Duration"
	case "JSON", "FieldMask":
		return e.runtime + ".JSON"
	// Wrapper-value WKTs: approximate as their nullable underlying scalar.
	case "DoubleValue", "FloatValue":
		return "graphql.Float"
	case "Int32Value", "UInt32Value":
		return "graphql.Int"
	case "Int64Value":
		return e.runtime + ".Int64"
	case "UInt64Value":
		return e.runtime + ".Uint64"
	case "BoolValue":
		return "graphql.Boolean"
	case "StringValue":
		return "graphql.String"
	case "BytesValue":
		return e.runtime + ".Bytes"
	default:
		return "graphql.String"
	}
}

// outBaseTypeExpr returns the graphql-go base Type expression (no list/nonnull) for an output field.
func (e *gqlEmitter) outBaseTypeExpr(field *protogen.Field) string {
	if field.Desc.IsMap() {
		return e.runtime + ".JSON"
	}
	switch field.Desc.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		fqn := string(field.Desc.Message().FullName())
		if sc, ok := wellKnownGQLType[fqn]; ok {
			return e.scalarTypeExpr(sc)
		}
		return objVar(gqlTypeName(field.Message))
	case protoreflect.EnumKind:
		return enumVar(gqlEnumName(field.Enum))
	default:
		return e.scalarTypeExpr(scalarForKind(field.Desc.Kind()))
	}
}

// outTypeExpr returns the full graphql-go Type expression for an output field
// (with list / non-null wrappers, mirroring fieldGQLType).
func (e *gqlEmitter) outTypeExpr(field *protogen.Field) string {
	if field.Desc.IsMap() {
		return e.runtime + ".JSON"
	}
	base := e.outBaseTypeExpr(field)
	if field.Desc.IsList() {
		switch field.Desc.Kind() {
		case protoreflect.MessageKind, protoreflect.GroupKind:
			fqn := string(field.Desc.Message().FullName())
			if _, wkt := wellKnownGQLType[fqn]; wkt {
				return fmt.Sprintf("graphql.NewList(graphql.NewNonNull(%s))", base)
			}
			return fmt.Sprintf("graphql.NewList(%s)", base)
		default:
			return fmt.Sprintf("graphql.NewList(graphql.NewNonNull(%s))", base)
		}
	}
	switch field.Desc.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return base // nullable
	default:
		if field.Desc.HasOptionalKeyword() {
			return base
		}
		return fmt.Sprintf("graphql.NewNonNull(%s)", base)
	}
}

// ── object/field resolver helpers ─────────────────────────────────────────────

// outFieldResolve returns the Go resolver-func literal for an output field.
func (e *gqlEmitter) outFieldResolve(ownerGo string, field *protogen.Field) string {
	getter := "obj.Get" + string(field.GoName) + "()"
	var val string
	switch {
	case field.Desc.IsMap():
		val = getter
	case field.Desc.Kind() == protoreflect.MessageKind || field.Desc.Kind() == protoreflect.GroupKind:
		fqn := string(field.Desc.Message().FullName())
		if sc, ok := wellKnownGQLType[fqn]; ok {
			switch sc {
			case "JSON":
				if field.Desc.IsList() {
					val = e.runtime + ".ToJSONList(" + getterAsProtoSlice(getter) + ")"
				} else {
					val = e.runtime + ".ToJSON(" + getter + ")"
				}
			default: // Timestamp, Duration, wrapper-value
				val = getter
			}
		} else {
			val = getter
		}
	default:
		val = getter
	}
	return fmt.Sprintf("func(p graphql.ResolveParams) (interface{}, error) {\n"+
		"\t\t\tobj, _ := p.Source.(*%s)\n"+
		"\t\t\tif obj == nil { return nil, nil }\n"+
		"\t\t\treturn %s, nil\n"+
		"\t\t}", ownerGo, val)
}

func getterAsProtoSlice(getter string) string { return getter }

// emit produces the full schema.go source.
func (e *gqlEmitter) emit(outPkg, runtimeImport string) (string, error) {
	var decls strings.Builder // var declarations (forward)
	var assigns strings.Builder
	var helpers strings.Builder // wrapper structs + decode funcs

	// Pre-register import for pb (always) by qualifying service + message idents.
	for _, svc := range e.g.Services {
		_ = svc
	}

	// 1. Enums.
	for _, en := range e.g.Enums {
		v := enumVar(gqlEnumName(en))
		e.typeVars[gqlEnumName(en)] = v
		e.decl(&decls, v, "*graphql.Enum")
		var vb strings.Builder
		fmt.Fprintf(&vb, "\t%s = graphql.NewEnum(graphql.EnumConfig{Name: %q, Values: graphql.EnumValueConfigMap{\n", v, gqlEnumName(en))
		for _, ev := range en.Values {
			fmt.Fprintf(&vb, "\t\t%q: &graphql.EnumValueConfig{Value: %s},\n", string(ev.Desc.Name()), e.qual(ev.GoIdent))
		}
		vb.WriteString("\t}})\n")
		assigns.WriteString(vb.String())
	}

	// 2. Output objects (message + empty), declared first.
	outMsgs := []*protogen.Message{}
	for _, msg := range e.g.Messages {
		mi := e.msgInfo[messageKey(msg)]
		if mi == nil || !mi.role.has(roleOutput) || msg.Desc.IsMapEntry() {
			continue
		}
		outMsgs = append(outMsgs, msg)
		v := objVar(gqlTypeName(msg))
		e.typeVars[gqlTypeName(msg)] = v
		e.decl(&decls, v, "*graphql.Object")
	}

	// 2b. Scalar-variant union wrapper objects + Go structs.
	for _, oi := range e.ois {
		if !oi.IsOutput {
			continue
		}
		for _, vr := range oi.Variants {
			if vr.IsMessage {
				continue
			}
			goType := scalarGoForVariant(vr)
			box := scalarWrapperBox{goType: vr.WrapperGoName, valGo: goType, objVar: objVar(vr.WrapperGoName), gqlName: vr.WrapperGoName}
			e.wrapperBox[vr.WrapperGoName] = box
			e.decl(&decls, box.objVar, "*graphql.Object")
			fmt.Fprintf(&helpers, "type %s struct{ Value %s }\n\n", box.goType, box.valGo)
		}
	}

	// 3. Assign output object thunks.
	for _, msg := range outMsgs {
		e.emitObject(&assigns, msg)
	}
	// 3b. Assign scalar wrapper objects.
	for _, oi := range e.ois {
		if !oi.IsOutput {
			continue
		}
		for _, vr := range oi.Variants {
			if vr.IsMessage {
				continue
			}
			box := e.wrapperBox[vr.WrapperGoName]
			fmt.Fprintf(&assigns, "\t%s = graphql.NewObject(graphql.ObjectConfig{Name: %q, Fields: graphql.Fields{\n", box.objVar, box.gqlName)
			fmt.Fprintf(&assigns, "\t\t\"value\": &graphql.Field{Type: graphql.NewNonNull(%s), Resolve: func(p graphql.ResolveParams) (interface{}, error) {\n", e.scalarTypeExpr(vr.GQLTypeName))
			fmt.Fprintf(&assigns, "\t\t\tif w, ok := p.Source.(%s); ok { return w.Value, nil }\n\t\t\treturn nil, nil\n\t\t}},\n", box.goType)
			assigns.WriteString("\t}})\n")
		}
	}

	// 4. Unions (after objects).
	for _, oi := range e.ois {
		if !oi.IsOutput {
			continue
		}
		e.emitUnion(&decls, &assigns, oi)
	}

	// 5. Input objects + decode funcs.
	e.emitInputs(&decls, &assigns, &helpers)

	// 6. Operation roots + schema.
	var roots strings.Builder
	e.emitRoots(&roots)

	// Build the body first (Server struct + helpers + NewSchema), then emit the
	// import block, dropping pb aliases that no body line references (an
	// imported-and-unused package is a compile error — e.g. emptypb when a WKT
	// Empty field maps to the JSON scalar and never names the pb type).
	var body strings.Builder
	body.WriteString("// Server holds the gRPC service implementations the schema delegates to.\n")
	body.WriteString("type Server struct {\n")
	for _, svc := range e.g.Services {
		fmt.Fprintf(&body, "\t%s %s\n", svc.GoName, e.qual(protogen.GoIdent{GoName: svc.GoName + "Server", GoImportPath: svc.Methods[0].Input.GoIdent.GoImportPath}))
	}
	body.WriteString("}\n\n")
	body.WriteString(helpers.String())
	body.WriteString("\n")
	body.WriteString("// NewSchema builds the executable GraphQL schema bound to srv.\n")
	body.WriteString("func NewSchema(srv *Server) (graphql.Schema, error) {\n")
	body.WriteString(decls.String())
	body.WriteString(assigns.String())
	body.WriteString(roots.String())
	body.WriteString("}\n")
	bodyStr := body.String()

	var sb strings.Builder
	fmt.Fprintf(&sb, "// Code generated by protoc-gen-go-graphql (graphql-go backend). DO NOT EDIT.\n")
	fmt.Fprintf(&sb, "package %s\n\n", outPkg)
	sb.WriteString("import (\n")
	sb.WriteString("\t\"github.com/graphql-go/graphql\"\n")
	fmt.Fprintf(&sb, "\t%s %q\n", e.runtime, runtimeImport)
	fmt.Fprintf(&sb, "\tpb %q\n", e.pbImport)
	for _, p := range e.extraImps {
		alias := e.pbAliases[p]
		if !strings.Contains(bodyStr, alias+".") {
			continue
		}
		fmt.Fprintf(&sb, "\t%s %q\n", alias, p)
	}
	sb.WriteString(")\n\n")
	sb.WriteString(bodyStr)

	return sb.String(), nil
}

// qualName qualifies a Go ident by import path (registering the import alias).
func (e *gqlEmitter) qualName(impPath protogen.GoImportPath, name string) string {
	return e.qual(protogen.GoIdent{GoName: name, GoImportPath: impPath})
}

// scalarGoForVariant returns the Go type of a scalar oneof variant's value.
func scalarGoForVariant(vr oneofVariant) string {
	if vr.PbScalarGoType != "" {
		return vr.PbScalarGoType
	}
	return "interface{}"
}

// emitObject emits the assignment for an output object var (FieldsThunk for cycles).
func (e *gqlEmitter) emitObject(assigns *strings.Builder, msg *protogen.Message) {
	v := objVar(gqlTypeName(msg))
	ownerGo := e.qual(msg.GoIdent)
	if isEmptyMessage(msg) {
		fmt.Fprintf(assigns, "\t%s = graphql.NewObject(graphql.ObjectConfig{Name: %q, Fields: graphql.Fields{\n", v, gqlTypeName(msg))
		assigns.WriteString("\t\t\"ok\": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: func(p graphql.ResolveParams) (interface{}, error) { return true, nil }},\n")
		assigns.WriteString("\t}})\n")
		return
	}
	// Index this message's output oneofs by proto name.
	outOneof := map[string]oneofInfo{}
	oneofFieldNames := map[string]bool{}
	for _, oi := range e.oneofsByMsg[messageKey(msg)] {
		if oi.IsOutput {
			outOneof[oi.ProtoName] = oi
			for _, vr := range oi.Variants {
				oneofFieldNames[vr.ProtoFieldName] = true
			}
		}
	}
	fmt.Fprintf(assigns, "\t%s = graphql.NewObject(graphql.ObjectConfig{Name: %q, Fields: graphql.FieldsThunk(func() graphql.Fields {\n\t\treturn graphql.Fields{\n", v, gqlTypeName(msg))
	emitted := map[string]bool{}
	for _, field := range msg.Fields {
		if fieldExcluded(field) {
			continue
		}
		protoName := string(field.Desc.Name())
		if oneofFieldNames[protoName] {
			if field.Oneof != nil && !field.Oneof.Desc.IsSynthetic() {
				oo := string(field.Oneof.Desc.Name())
				if oi, ok := outOneof[oo]; ok && !emitted[oo] {
					emitted[oo] = true
					fmt.Fprintf(assigns, "\t\t\t%q: &graphql.Field{Type: %s, Resolve: %s},\n",
						oi.GQLFieldName, unionVar(oi.UnionGQLName), e.oneofResolve(ownerGo, msg, oi))
				}
			}
			continue
		}
		fmt.Fprintf(assigns, "\t\t\t%q: &graphql.Field{Type: %s, Resolve: %s},\n",
			gqlFieldName(field), e.outTypeExpr(field), e.outFieldResolve(ownerGo, field))
	}
	assigns.WriteString("\t\t}\n\t})})\n")
}

// oneofResolve emits the resolver for an output-oneof union field.
func (e *gqlEmitter) oneofResolve(ownerGo string, msg *protogen.Message, oi oneofInfo) string {
	var b strings.Builder
	b.WriteString("func(p graphql.ResolveParams) (interface{}, error) {\n")
	fmt.Fprintf(&b, "\t\t\tobj, _ := p.Source.(*%s)\n\t\t\tif obj == nil { return nil, nil }\n", ownerGo)
	fmt.Fprintf(&b, "\t\t\tswitch v := obj.Get%s().(type) {\n", oi.OneofGoName)
	for _, vr := range oi.Variants {
		caseType := e.qualName(msg.GoIdent.GoImportPath, vr.WrapperPbField)
		if vr.IsMessage {
			fmt.Fprintf(&b, "\t\t\tcase *%s:\n\t\t\t\treturn v.%s, nil\n", caseType, vr.GoFieldName)
		} else {
			fmt.Fprintf(&b, "\t\t\tcase *%s:\n\t\t\t\treturn %s{Value: v.%s}, nil\n", caseType, vr.WrapperGoName, vr.GoFieldName)
		}
	}
	b.WriteString("\t\t\t}\n\t\t\treturn nil, nil\n\t\t}")
	return b.String()
}

// emitUnion emits the union var declaration + assignment.
func (e *gqlEmitter) emitUnion(decls, assigns *strings.Builder, oi oneofInfo) {
	uv := unionVar(oi.UnionGQLName)
	e.decl(decls, uv, "*graphql.Union")
	// Two oneof variants can share a payload Go type (e.g. mysql + mariadb both
	// *MySqlParams). graphql-go binds union members by Go type, so a shared type
	// collapses to one member / one ResolveType case — dedup to keep the schema
	// and the generated type switch valid.
	var members []string
	seenMember := map[string]bool{}
	for _, vr := range oi.Variants {
		var ov string
		if vr.IsMessage {
			ov = objVar(gqlTypeName(vr.Msg))
		} else {
			ov = e.wrapperBox[vr.WrapperGoName].objVar
		}
		if seenMember[ov] {
			continue
		}
		seenMember[ov] = true
		members = append(members, ov)
	}
	fmt.Fprintf(assigns, "\t%s = graphql.NewUnion(graphql.UnionConfig{Name: %q, Types: []*graphql.Object{%s},\n",
		uv, oi.UnionGQLName, strings.Join(members, ", "))
	assigns.WriteString("\t\tResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {\n\t\t\tswitch p.Value.(type) {\n")
	seenCase := map[string]bool{}
	for _, vr := range oi.Variants {
		var caseType, ov string
		if vr.IsMessage {
			caseType = "*" + e.qual(vr.Msg.GoIdent)
			ov = objVar(gqlTypeName(vr.Msg))
		} else {
			caseType = vr.WrapperGoName
			ov = e.wrapperBox[vr.WrapperGoName].objVar
		}
		if seenCase[caseType] {
			continue
		}
		seenCase[caseType] = true
		fmt.Fprintf(assigns, "\t\t\tcase %s:\n\t\t\t\treturn %s\n", caseType, ov)
	}
	assigns.WriteString("\t\t\t}\n\t\t\treturn nil\n\t\t}})\n")
}

// ── input types + decode ──────────────────────────────────────────────────────

func (e *gqlEmitter) inputRefName(msg *protogen.Message) string {
	mi := e.msgInfo[messageKey(msg)]
	if mi != nil && mi.isRequest {
		return inputTypeName(msg, e.msgInfo)
	}
	return gqlTypeName(msg) + "Input"
}

// inputBaseTypeExpr returns the graphql-go base input Type expression for a field.
func (e *gqlEmitter) inputBaseTypeExpr(field *protogen.Field) string {
	switch field.Desc.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		fqn := string(field.Desc.Message().FullName())
		if sc, ok := wellKnownGQLType[fqn]; ok {
			return e.scalarTypeExpr(sc)
		}
		return inputVar(e.inputRefName(field.Message))
	case protoreflect.EnumKind:
		return enumVar(gqlEnumName(field.Enum))
	default:
		return e.scalarTypeExpr(scalarForKind(field.Desc.Kind()))
	}
}

func (e *gqlEmitter) inputTypeExpr(field *protogen.Field) string {
	base := e.inputBaseTypeExpr(field)
	if field.Desc.IsList() {
		switch field.Desc.Kind() {
		case protoreflect.MessageKind, protoreflect.GroupKind:
			return fmt.Sprintf("graphql.NewList(%s)", base)
		default:
			return fmt.Sprintf("graphql.NewList(graphql.NewNonNull(%s))", base)
		}
	}
	switch field.Desc.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return base
	default:
		if field.Desc.HasOptionalKeyword() {
			return base
		}
		return fmt.Sprintf("graphql.NewNonNull(%s)", base)
	}
}

// emitInputs emits input object vars + assignments + decode funcs for every
// input-role message and @oneOf input.
func (e *gqlEmitter) emitInputs(decls, assigns, helpers *strings.Builder) {
	seen := map[string]bool{}
	for _, msg := range e.g.Messages {
		mi := e.msgInfo[messageKey(msg)]
		if mi == nil || !mi.role.has(roleInput) || msg.Desc.IsMapEntry() {
			continue
		}
		name := e.inputRefName(msg)
		if seen[name] {
			continue
		}
		seen[name] = true
		e.emitInputObject(decls, assigns, msg, name)
		e.emitDecodeFunc(helpers, msg, name)
		// @oneOf input objects for this message's input oneofs.
		for _, oi := range e.oneofsByMsg[messageKey(msg)] {
			if !seen[e.oneofInputName(oi)] {
				seen[e.oneofInputName(oi)] = true
				e.emitOneofInputObject(decls, assigns, oi)
			}
		}
	}
}

func (e *gqlEmitter) emitInputObject(decls, assigns *strings.Builder, msg *protogen.Message, name string) {
	v := inputVar(name)
	e.decl(decls, v, "*graphql.InputObject")
	fmt.Fprintf(assigns, "\t%s = graphql.NewInputObject(graphql.InputObjectConfig{Name: %q, Fields: graphql.InputObjectConfigFieldMapThunk(func() graphql.InputObjectConfigFieldMap {\n\t\treturn graphql.InputObjectConfigFieldMap{\n", v, name)
	lines := e.inputFieldLines(msg)
	if len(lines) == 0 {
		assigns.WriteString("\t\t\t\"_empty\": &graphql.InputObjectFieldConfig{Type: graphql.Boolean},\n")
	}
	for _, l := range lines {
		assigns.WriteString(l)
	}
	assigns.WriteString("\t\t}\n\t})})\n")
}

// inputFieldLines returns the InputObjectConfigFieldMap entries for a message.
func (e *gqlEmitter) inputFieldLines(msg *protogen.Message) []string {
	var lines []string
	oneofFieldNames := map[string]bool{}
	oneofByProto := map[string]oneofInfo{}
	for _, oi := range e.oneofsByMsg[messageKey(msg)] {
		// In input context every non-synthetic oneof of an input-role message is an
		// @oneOf input (collectOneofsGraph only flags IsInput for top-level requests).
		oneofByProto[oi.ProtoName] = oi
		for _, vr := range oi.Variants {
			oneofFieldNames[vr.ProtoFieldName] = true
		}
	}
	emitted := map[string]bool{}
	for _, field := range msg.Fields {
		if field.Desc.IsMap() || fieldExcluded(field) {
			continue
		}
		pn := string(field.Desc.Name())
		if oneofFieldNames[pn] {
			if field.Oneof != nil && !field.Oneof.Desc.IsSynthetic() {
				oo := string(field.Oneof.Desc.Name())
				if oi, ok := oneofByProto[oo]; ok && !emitted[oo] {
					emitted[oo] = true
					lines = append(lines, fmt.Sprintf("\t\t\t%q: &graphql.InputObjectFieldConfig{Type: %s},\n", oi.GQLFieldName, inputVar(e.oneofInputName(oi))))
				}
			}
			continue
		}
		lines = append(lines, fmt.Sprintf("\t\t\t%q: &graphql.InputObjectFieldConfig{Type: %s},\n", gqlFieldName(field), e.inputTypeExpr(field)))
	}
	return lines
}

func (e *gqlEmitter) emitOneofInputObject(decls, assigns *strings.Builder, oi oneofInfo) {
	nm := e.oneofInputName(oi)
	v := inputVar(nm)
	e.decl(decls, v, "*graphql.InputObject")
	fmt.Fprintf(assigns, "\t%s = graphql.NewInputObject(graphql.InputObjectConfig{Name: %q, Fields: graphql.InputObjectConfigFieldMapThunk(func() graphql.InputObjectConfigFieldMap {\n\t\treturn graphql.InputObjectConfigFieldMap{\n", v, nm)
	for _, vr := range oi.Variants {
		fmt.Fprintf(assigns, "\t\t\t%q: &graphql.InputObjectFieldConfig{Type: %s},\n", fieldName(vr.ProtoFieldName), e.oneofInputVariantType(vr))
	}
	assigns.WriteString("\t\t}\n\t})})\n")
}

// oneofInputName returns the @oneOf input type name for an input oneof. When the
// oneof's message is ALSO an output type (its oneof becomes a union of the same
// base name), the input gets an "Input" suffix so the two named types don't clash.
func (e *gqlEmitter) oneofInputName(oi oneofInfo) string {
	name := oi.UnionGQLName
	if mi := e.msgInfo[messageKey(oi.Msg)]; mi != nil && mi.role.has(roleOutput) {
		name += "Input"
	}
	return name
}

func (e *gqlEmitter) oneofInputVariantType(vr oneofVariant) string {
	if vr.IsMessage && vr.Msg != nil {
		return inputVar(e.inputRefName(vr.Msg))
	}
	return e.scalarTypeExpr(vr.GQLTypeName)
}

// emitDecodeFunc emits decode_<name>(m) *pb.Msg.
func (e *gqlEmitter) emitDecodeFunc(helpers *strings.Builder, msg *protogen.Message, name string) {
	ownerGo := e.qual(msg.GoIdent)
	fmt.Fprintf(helpers, "func decode_%s(m map[string]interface{}) *%s {\n", sanitizeIdent(name), ownerGo)
	fmt.Fprintf(helpers, "\tout := &%s{}\n\tif m == nil { return out }\n", ownerGo)
	oneofFieldNames := map[string]bool{}
	oneofByProto := map[string]oneofInfo{}
	for _, oi := range e.oneofsByMsg[messageKey(msg)] {
		// In input context every non-synthetic oneof of an input-role message is an
		// @oneOf input (collectOneofsGraph only flags IsInput for top-level requests).
		oneofByProto[oi.ProtoName] = oi
		for _, vr := range oi.Variants {
			oneofFieldNames[vr.ProtoFieldName] = true
		}
	}
	emitted := map[string]bool{}
	for _, field := range msg.Fields {
		if field.Desc.IsMap() || fieldExcluded(field) {
			continue
		}
		pn := string(field.Desc.Name())
		if oneofFieldNames[pn] {
			if field.Oneof != nil && !field.Oneof.Desc.IsSynthetic() {
				oo := string(field.Oneof.Desc.Name())
				if oi, ok := oneofByProto[oo]; ok && !emitted[oo] {
					emitted[oo] = true
					e.emitOneofDecode(helpers, msg, oi)
				}
			}
			continue
		}
		e.emitFieldDecode(helpers, msg, field)
	}
	helpers.WriteString("\treturn out\n}\n\n")
}

func (e *gqlEmitter) emitFieldDecode(h *strings.Builder, msg *protogen.Message, field *protogen.Field) {
	gql := gqlFieldName(field)
	goName := string(field.GoName)
	key := fmt.Sprintf("m[%q]", gql)
	if field.Desc.IsList() {
		// list: iterate []interface{}
		fmt.Fprintf(h, "\tif arr, ok := %s.([]interface{}); ok {\n", key)
		switch field.Desc.Kind() {
		case protoreflect.MessageKind, protoreflect.GroupKind:
			fqn := string(field.Desc.Message().FullName())
			if _, wkt := wellKnownGQLType[fqn]; wkt {
				h.WriteString("\t\t_ = arr\n") // WKT-JSON list inputs unsupported
			} else {
				fmt.Fprintf(h, "\t\tfor _, it := range arr {\n\t\t\tif mm, ok := it.(map[string]interface{}); ok {\n\t\t\t\tout.%s = append(out.%s, decode_%s(mm))\n\t\t\t}\n\t\t}\n", goName, goName, sanitizeIdent(e.inputRefName(field.Message)))
			}
		case protoreflect.EnumKind:
			fmt.Fprintf(h, "\t\tfor _, it := range arr {\n\t\t\tif ev, ok := it.(%s); ok { out.%s = append(out.%s, ev) }\n\t\t}\n", e.qual(field.Enum.GoIdent), goName, goName)
		default:
			fmt.Fprintf(h, "\t\tfor _, it := range arr {\n\t\t\tout.%s = append(out.%s, %s)\n\t\t}\n", goName, goName, e.scalarConvExpr(field, "it"))
		}
		h.WriteString("\t}\n")
		return
	}
	switch field.Desc.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		fqn := string(field.Desc.Message().FullName())
		if _, wkt := wellKnownGQLType[fqn]; wkt {
			if _, isJSON := map[string]bool{"JSON": false}[fqn]; isJSON {
				return
			}
			// Timestamp/Duration/wrapper: assert the concrete proto pointer type.
			fmt.Fprintf(h, "\tif tv, ok := %s.(*%s); ok { out.%s = tv }\n", key, e.qual(field.Message.GoIdent), goName)
			return
		}
		fmt.Fprintf(h, "\tif mm, ok := %s.(map[string]interface{}); ok { out.%s = decode_%s(mm) }\n", key, goName, sanitizeIdent(e.inputRefName(field.Message)))
	case protoreflect.EnumKind:
		fmt.Fprintf(h, "\tif ev, ok := %s.(%s); ok { out.%s = ev }\n", key, e.qual(field.Enum.GoIdent), goName)
	default:
		if field.Desc.HasOptionalKeyword() {
			fmt.Fprintf(h, "\tif _, ok := %s; ok { v := %s; out.%s = &v }\n", key, e.scalarConvExpr(field, key), goName)
		} else {
			fmt.Fprintf(h, "\tout.%s = %s\n", goName, e.scalarConvExpr(field, key))
		}
	}
}

// scalarConvExpr returns a graphqlrt.As* expression converting an input value to a pb scalar.
func (e *gqlEmitter) scalarConvExpr(field *protogen.Field, expr string) string {
	switch field.Desc.Kind() {
	case protoreflect.StringKind:
		return e.runtime + ".AsString(" + expr + ")"
	case protoreflect.BoolKind:
		return e.runtime + ".AsBool(" + expr + ")"
	case protoreflect.BytesKind:
		return e.runtime + ".AsBytes(" + expr + ")"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return e.runtime + ".AsInt32(" + expr + ")"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return e.runtime + ".AsUint32(" + expr + ")"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return e.runtime + ".AsInt64(" + expr + ")"
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return e.runtime + ".AsUint64(" + expr + ")"
	case protoreflect.FloatKind:
		return e.runtime + ".AsFloat32(" + expr + ")"
	case protoreflect.DoubleKind:
		return e.runtime + ".AsFloat64(" + expr + ")"
	default:
		return e.runtime + ".AsString(" + expr + ")"
	}
}

// emitOneofDecode emits decode for an input oneof field.
func (e *gqlEmitter) emitOneofDecode(h *strings.Builder, msg *protogen.Message, oi oneofInfo) {
	// Look up each variant's protogen field (for enum type assertions).
	fieldByProto := map[string]*protogen.Field{}
	for _, f := range msg.Fields {
		fieldByProto[string(f.Desc.Name())] = f
	}
	fmt.Fprintf(h, "\tif ov, ok := m[%q].(map[string]interface{}); ok {\n", oi.GQLFieldName)
	h.WriteString("\t\tswitch {\n")
	for _, vr := range oi.Variants {
		gqlv := fieldName(vr.ProtoFieldName)
		caseType := e.qualName(msg.GoIdent.GoImportPath, vr.WrapperPbField)
		f := fieldByProto[vr.ProtoFieldName]
		switch {
		case vr.IsMessage:
			fmt.Fprintf(h, "\t\tcase ov[%q] != nil:\n\t\t\tif mm, ok := ov[%q].(map[string]interface{}); ok { out.%s = &%s{%s: decode_%s(mm)} }\n",
				gqlv, gqlv, oi.OneofGoName, caseType, vr.GoFieldName, sanitizeIdent(e.inputRefName(vr.Msg)))
		case f != nil && f.Desc.Kind() == protoreflect.EnumKind:
			fmt.Fprintf(h, "\t\tcase ov[%q] != nil:\n\t\t\tif ev, ok := ov[%q].(%s); ok { out.%s = &%s{%s: ev} }\n",
				gqlv, gqlv, e.qual(f.Enum.GoIdent), oi.OneofGoName, caseType, vr.GoFieldName)
		default:
			fmt.Fprintf(h, "\t\tcase ov[%q] != nil:\n\t\t\tout.%s = &%s{%s: %s}\n",
				gqlv, oi.OneofGoName, caseType, vr.GoFieldName, e.scalarConvExprKind(vr, fmt.Sprintf("ov[%q]", gqlv)))
		}
	}
	h.WriteString("\t\t}\n\t}\n")
}

func (e *gqlEmitter) scalarConvExprKind(vr oneofVariant, expr string) string {
	switch vr.PbScalarGoType {
	case "string":
		return e.runtime + ".AsString(" + expr + ")"
	case "bool":
		return e.runtime + ".AsBool(" + expr + ")"
	case "[]byte":
		return e.runtime + ".AsBytes(" + expr + ")"
	case "int32":
		return e.runtime + ".AsInt32(" + expr + ")"
	case "uint32":
		return e.runtime + ".AsUint32(" + expr + ")"
	case "int64":
		return e.runtime + ".AsInt64(" + expr + ")"
	case "uint64":
		return e.runtime + ".AsUint64(" + expr + ")"
	case "float32":
		return e.runtime + ".AsFloat32(" + expr + ")"
	case "float64":
		return e.runtime + ".AsFloat64(" + expr + ")"
	default:
		return e.runtime + ".AsString(" + expr + ")"
	}
}

// ── operation roots ───────────────────────────────────────────────────────────

func (e *gqlEmitter) emitRoots(roots *strings.Builder) {
	type opField struct{ name, body string }
	var queries, mutations, subs []opField
	for _, svc := range e.g.Services {
		for _, m := range includedMethods(svc) {
			op := operationType(m)
			f := e.emitOpField(svc, m, op)
			switch op {
			case "Query":
				queries = append(queries, opField{methodFieldName(m), f})
			case "Mutation":
				mutations = append(mutations, opField{methodFieldName(m), f})
			case "Subscription":
				subs = append(subs, opField{methodFieldName(m), f})
			}
		}
	}
	emitRoot := func(varName, gqlName string, fs []opField) bool {
		if len(fs) == 0 {
			return false
		}
		fmt.Fprintf(roots, "\t%s := graphql.NewObject(graphql.ObjectConfig{Name: %q, Fields: graphql.Fields{\n", varName, gqlName)
		for _, f := range fs {
			fmt.Fprintf(roots, "\t\t%q: %s,\n", f.name, f.body)
		}
		roots.WriteString("\t}})\n")
		return true
	}
	hasQ := emitRoot("queryRoot", "Query", queries)
	hasM := emitRoot("mutationRoot", "Mutation", mutations)
	hasS := emitRoot("subscriptionRoot", "Subscription", subs)
	roots.WriteString("\tcfg := graphql.SchemaConfig{}\n")
	if hasQ {
		roots.WriteString("\tcfg.Query = queryRoot\n")
	}
	if hasM {
		roots.WriteString("\tcfg.Mutation = mutationRoot\n")
	}
	if hasS {
		roots.WriteString("\tcfg.Subscription = subscriptionRoot\n")
	}
	// Register every declared type (also references each var, avoiding
	// "declared and not used" for types not reachable from a root operation).
	if len(e.declaredVars) > 0 {
		roots.WriteString("\tcfg.Types = []graphql.Type{" + strings.Join(e.declaredVars, ", ") + "}\n")
	}
	roots.WriteString("\treturn graphql.NewSchema(cfg)\n")
}

// emitOpField emits the &graphql.Field{...} literal for one operation.
func (e *gqlEmitter) emitOpField(svc *protogen.Service, m *protogen.Method, op string) string {
	reqGo := e.qual(m.Input.GoIdent)
	respGo := e.qual(m.Output.GoIdent)
	empty := isEmptyMessage(m.Input)
	var args string
	if !empty {
		args = e.opArgs(m.Input)
	}
	reqExpr := fmt.Sprintf("&%s{}", reqGo)
	if !empty {
		reqExpr = fmt.Sprintf("decode_%s(p.Args)", sanitizeIdent(e.inputRefName(m.Input)))
	}
	var b strings.Builder
	switch op {
	case "Query", "Mutation":
		fmt.Fprintf(&b, "&graphql.Field{Type: %s, ", objVar(gqlTypeName(m.Output)))
		if args != "" {
			fmt.Fprintf(&b, "Args: %s, ", args)
		}
		b.WriteString("Resolve: func(p graphql.ResolveParams) (interface{}, error) {\n")
		fmt.Fprintf(&b, "\t\t\treq := %s\n", reqExpr)
		fmt.Fprintf(&b, "\t\t\tresp, err := srv.%s.%s(p.Context, req)\n", svc.GoName, m.GoName)
		fmt.Fprintf(&b, "\t\t\tif err != nil { return nil, %s.GraphQLError(p.Context, err) }\n\t\t\treturn resp, nil\n\t\t}}", e.runtime)
	case "Subscription":
		fmt.Fprintf(&b, "&graphql.Field{Type: %s, ", objVar(gqlTypeName(m.Output)))
		if args != "" {
			fmt.Fprintf(&b, "Args: %s, ", args)
		}
		b.WriteString("Resolve: func(p graphql.ResolveParams) (interface{}, error) { return p.Source, nil },\n")
		b.WriteString("\t\t\tSubscribe: func(p graphql.ResolveParams) (interface{}, error) {\n")
		fmt.Fprintf(&b, "\t\t\t\treq := %s\n", reqExpr)
		fmt.Fprintf(&b, "\t\t\t\treturn %s.PumpServerStream[%s](p.Context, func(ss *%s.StreamServer[%s]) error {\n", e.runtime, respGo, e.runtime, respGo)
		fmt.Fprintf(&b, "\t\t\t\t\treturn srv.%s.%s(req, ss)\n\t\t\t\t}), nil\n\t\t\t}}", svc.GoName, m.GoName)
	}
	return b.String()
}

// opArgs emits the FieldConfigArgument for a request message's top-level fields.
func (e *gqlEmitter) opArgs(msg *protogen.Message) string {
	var b strings.Builder
	b.WriteString("graphql.FieldConfigArgument{\n")
	for _, l := range e.inputFieldLines(msg) {
		// reuse inputFieldLines but wrap as ArgumentConfig
		l = strings.Replace(l, "&graphql.InputObjectFieldConfig{", "&graphql.ArgumentConfig{", 1)
		b.WriteString(l)
	}
	b.WriteString("\t\t}")
	return b.String()
}
