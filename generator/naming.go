package generator

import "strings"

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
