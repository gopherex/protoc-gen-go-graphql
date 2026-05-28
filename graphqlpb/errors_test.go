package graphqlpb

import (
	"context"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"
)

func TestGraphQLErrorDetails(t *testing.T) {
	info := &errdetails.ErrorInfo{
		Reason:   "RATE_LIMIT",
		Domain:   "api.example.com",
		Metadata: map[string]string{"limit": "100"},
	}
	st, err := status.New(codes.ResourceExhausted, "slow down").
		WithDetails(protoadapt.MessageV1Of(info))
	if err != nil {
		t.Fatal(err)
	}
	gqlErr := GraphQLError(context.Background(), st.Err())

	det, ok := gqlErr.Extensions["details"].([]any)
	if !ok || len(det) != 1 {
		t.Fatalf("details = %#v", gqlErr.Extensions["details"])
	}
	obj, ok := det[0].(map[string]any)
	if !ok {
		t.Fatalf("detail not a structured object: %T", det[0])
	}
	if obj["@type"] != "google.rpc.ErrorInfo" {
		t.Fatalf("@type = %v", obj["@type"])
	}
	if obj["reason"] != "RATE_LIMIT" {
		t.Fatalf("reason = %v", obj["reason"])
	}
	md, ok := obj["metadata"].(map[string]any)
	if !ok || md["limit"] != "100" {
		t.Fatalf("metadata = %v", obj["metadata"])
	}
}

func TestGraphQLErrorCode(t *testing.T) {
	in := status.Error(codes.NotFound, "book missing")
	gqlErr := GraphQLError(context.Background(), in)
	if gqlErr == nil {
		t.Fatal("expected non-nil error")
	}
	if gqlErr.Message != "book missing" {
		t.Fatalf("message = %q", gqlErr.Message)
	}
	if gqlErr.Extensions["code"] != "NOT_FOUND" {
		t.Fatalf("code = %v", gqlErr.Extensions["code"])
	}
}

func TestGraphQLErrorNil(t *testing.T) {
	if GraphQLError(context.Background(), nil) != nil {
		t.Fatal("nil error must map to nil")
	}
}

func TestGraphQLErrorNonStatus(t *testing.T) {
	// A plain (non-gRPC) error maps to UNKNOWN per status.FromError convention.
	gqlErr := GraphQLError(context.Background(), context.Canceled)
	if gqlErr == nil || gqlErr.Extensions["code"] != "UNKNOWN" {
		t.Fatalf("plain error code = %v", gqlErr.Extensions["code"])
	}
}

func TestCodeNameMapping(t *testing.T) {
	cases := map[codes.Code]string{
		codes.OK:                 "OK",
		codes.InvalidArgument:    "INVALID_ARGUMENT",
		codes.DeadlineExceeded:   "DEADLINE_EXCEEDED",
		codes.PermissionDenied:   "PERMISSION_DENIED",
		codes.FailedPrecondition: "FAILED_PRECONDITION",
		codes.Unauthenticated:    "UNAUTHENTICATED",
	}
	for c, want := range cases {
		if got := codeName(c); got != want {
			t.Fatalf("codeName(%v) = %q, want %q", c, got, want)
		}
	}
}
