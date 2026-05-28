package graphqlpb

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// GraphQLError maps a gRPC status error to a GraphQL error: the codes.Code
// canonical name goes into extensions["code"], the status message becomes the
// GraphQL error message, and any status details (google.rpc.* messages such as
// ErrorInfo/BadRequest, including their error metadata) are protojson-marshaled
// into structured objects under extensions["details"], each tagged with its
// proto type via "@type". Returns nil for a nil error.
//
// Note: gRPC transport metadata/trailers (grpc.SetHeader/SetTrailer) are NOT
// surfaced — the generated resolvers delegate to the gRPC server in-process
// (no transport), so trailers do not flow. Carry error metadata in
// google.rpc.ErrorInfo.metadata (a status detail) instead; that IS surfaced.
func GraphQLError(ctx context.Context, err error) *gqlerror.Error {
	if err == nil {
		return nil
	}
	st, _ := status.FromError(err)
	gqlErr := gqlerror.WrapPath(graphql.GetPath(ctx), err)
	gqlErr.Message = st.Message()
	gqlErr.Extensions = map[string]any{"code": codeName(st.Code())}
	if d := st.Details(); len(d) > 0 {
		details := make([]any, 0, len(d))
		for _, item := range d {
			details = append(details, structuredDetail(item))
		}
		gqlErr.Extensions["details"] = details
	}
	return gqlErr
}

// structuredDetail renders one status detail as protojson-aligned data. Message
// details become a JSON object tagged with "@type"; non-message details (rare)
// fall back to a string.
func structuredDetail(item any) any {
	m, ok := item.(proto.Message)
	if !ok {
		return fmt.Sprintf("%v", item)
	}
	b, err := protojson.Marshal(m)
	if err != nil {
		return fmt.Sprintf("%v", item)
	}
	typeName := string(m.ProtoReflect().Descriptor().FullName())
	var decoded any
	if err := json.Unmarshal(b, &decoded); err != nil {
		return map[string]any{"@type": typeName}
	}
	if obj, ok := decoded.(map[string]any); ok {
		obj["@type"] = typeName
		return obj
	}
	// Scalar/array protojson (e.g. a well-known wrapper used as a detail).
	return map[string]any{"@type": typeName, "value": decoded}
}

// codeName returns the canonical SCREAMING_SNAKE_CASE name of a gRPC status code,
// as used across gRPC implementations and expected by GraphQL clients. The
// stdlib codes.Code.String() returns Go-cased names (e.g. "NotFound"), so this
// explicit mapping is required.
func codeName(c codes.Code) string {
	switch c {
	case codes.OK:
		return "OK"
	case codes.Canceled:
		return "CANCELLED"
	case codes.Unknown:
		return "UNKNOWN"
	case codes.InvalidArgument:
		return "INVALID_ARGUMENT"
	case codes.DeadlineExceeded:
		return "DEADLINE_EXCEEDED"
	case codes.NotFound:
		return "NOT_FOUND"
	case codes.AlreadyExists:
		return "ALREADY_EXISTS"
	case codes.PermissionDenied:
		return "PERMISSION_DENIED"
	case codes.ResourceExhausted:
		return "RESOURCE_EXHAUSTED"
	case codes.FailedPrecondition:
		return "FAILED_PRECONDITION"
	case codes.Aborted:
		return "ABORTED"
	case codes.OutOfRange:
		return "OUT_OF_RANGE"
	case codes.Unimplemented:
		return "UNIMPLEMENTED"
	case codes.Internal:
		return "INTERNAL"
	case codes.Unavailable:
		return "UNAVAILABLE"
	case codes.DataLoss:
		return "DATA_LOSS"
	case codes.Unauthenticated:
		return "UNAUTHENTICATED"
	default:
		return "UNKNOWN"
	}
}
