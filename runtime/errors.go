package runtime

import (
	"context"
	"fmt"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GraphQLError maps a gRPC status error to a GraphQL error: the codes.Code
// canonical name goes into extensions["code"], the status message becomes the
// GraphQL error message, and any status details are stringified into
// extensions["details"]. Returns nil for a nil error.
func GraphQLError(ctx context.Context, err error) *gqlerror.Error {
	if err == nil {
		return nil
	}
	st, _ := status.FromError(err)
	gqlErr := gqlerror.WrapPath(graphql.GetPath(ctx), err)
	gqlErr.Message = st.Message()
	gqlErr.Extensions = map[string]any{"code": codeName(st.Code())}
	if d := st.Details(); len(d) > 0 {
		details := make([]string, 0, len(d))
		for _, item := range d {
			details = append(details, fmt.Sprintf("%v", item))
		}
		gqlErr.Extensions["details"] = details
	}
	return gqlErr
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
