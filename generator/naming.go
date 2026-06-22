package generator

import (
	"strings"

	"github.com/99designs/gqlgen/codegen/templates"
)

// goResolverName returns the Go method name gqlgen generates for a GraphQL field
// or operation of the given (camelCase) name. gqlgen runs every schema field name
// through templates.ToGo, which applies Go initialisms (API, CPU, ID, URL, …).
// The generator MUST use the SAME function for the resolver methods it emits, or
// they won't satisfy gqlgen's generated resolver interfaces (e.g. a field
// "cpuUtilizationPercent" becomes "CPUUtilizationPercent", not "CpuUtilizationPercent").
func goResolverName(gqlName string) string {
	return templates.ToGo(gqlName)
}

// fieldName converts proto snake_case to GraphQL camelCase.
func fieldName(proto string) string {
	parts := strings.Split(proto, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

func inputName(typeName string) string { return typeName + "Input" }

func operationFieldName(rpcName string) string {
	if rpcName == "" {
		return rpcName
	}
	return strings.ToLower(rpcName[:1]) + rpcName[1:]
}
