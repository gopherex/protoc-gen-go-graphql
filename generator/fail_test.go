package generator

import (
	"strings"
	"testing"
)

func TestCheckStreamingRejectsBidi(t *testing.T) {
	// methodStub mimics the streaming flags of a protogen.Method.
	err := checkStreaming("Chat", "Talk", true /*client*/, true /*server*/)
	if err == nil || !strings.Contains(err.Error(), "bidi") {
		t.Fatalf("want bidi error, got %v", err)
	}
}

func TestCheckStreamingRejectsClientStream(t *testing.T) {
	err := checkStreaming("Upload", "Send", true, false)
	if err == nil || !strings.Contains(err.Error(), "client-streaming") {
		t.Fatalf("want client-stream error, got %v", err)
	}
}

func TestCheckStreamingAllowsServerStream(t *testing.T) {
	if err := checkStreaming("Library", "WatchBooks", false, true); err != nil {
		t.Fatalf("server stream should be allowed: %v", err)
	}
}
