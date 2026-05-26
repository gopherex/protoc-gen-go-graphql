package generator

import "fmt"

// checkStreaming returns a fail-fast error for unsupported streaming shapes.
func checkStreaming(service, method string, client, server bool) error {
	switch {
	case client && server:
		return fmt.Errorf("%s.%s: bidi-streaming rpc is not supported (GraphQL subscriptions are server->client only)", service, method)
	case client:
		return fmt.Errorf("%s.%s: client-streaming rpc is not supported", service, method)
	default:
		return nil
	}
}
