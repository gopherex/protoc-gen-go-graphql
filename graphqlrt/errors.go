package graphqlrt

import (
	"context"
	"encoding/json"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// unmarshalJSON decodes protojson bytes into a plain Go value (map/slice/scalar),
// or returns nil if the bytes are not valid JSON.
func unmarshalJSON(b []byte) interface{} {
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil
	}
	return v
}

// statusError adapts a gRPC status to graphql-go's gqlerrors.ExtendedError, so
// the GraphQL error carries extensions.code (SCREAMING_SNAKE code name) and
// extensions.details (protojson-marshaled status details). It satisfies the
// `Error() string` + `Extensions() map[string]interface{}` interface graphql-go
// looks for when formatting resolver errors.
type statusError struct {
	msg        string
	code       string
	details    []interface{}
	underlying error
}

func (e *statusError) Error() string { return e.msg }

func (e *statusError) Extensions() map[string]interface{} {
	ext := map[string]interface{}{"code": e.code}
	if len(e.details) > 0 {
		ext["details"] = e.details
	}
	return ext
}

func (e *statusError) Unwrap() error { return e.underlying }

// GraphQLError maps a gRPC error returned by a delegated server method into a
// GraphQL-surfaceable error. A non-status error is returned unchanged. The
// in-process delegation has no transport, so header/trailer metadata never
// travels — error metadata must live in a google.rpc.* status detail
// (ErrorInfo.metadata), which IS surfaced here under extensions.details.
func GraphQLError(_ context.Context, err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	se := &statusError{
		msg:        st.Message(),
		code:       codeName(st.Code()),
		underlying: err,
	}
	for _, d := range st.Proto().GetDetails() {
		b, merr := protojson.Marshal(d)
		if merr != nil {
			continue
		}
		var obj interface{}
		if json := unmarshalJSON(b); json != nil {
			obj = json
		} else {
			obj = string(b)
		}
		se.details = append(se.details, obj)
	}
	return se
}

// codeName renders a gRPC code as its canonical SCREAMING_SNAKE_CASE name
// (e.g. codes.NotFound → "NOT_FOUND").
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
