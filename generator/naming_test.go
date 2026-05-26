package generator

import "testing"

func TestFieldName(t *testing.T) {
	if got := fieldName("published_at"); got != "publishedAt" {
		t.Fatalf("got %q", got)
	}
}

func TestInputName(t *testing.T) {
	if got := inputName("Book"); got != "BookInput" {
		t.Fatalf("got %q", got)
	}
}

func TestOperationFieldName(t *testing.T) {
	if got := operationFieldName("GetBook"); got != "getBook" {
		t.Fatalf("got %q", got)
	}
}
