package main

import (
	"reflect"
	"testing"
)

func TestSplitCommandList_TopLevelOnly(t *testing.T) {
	got := splitCommandList("go vet ./...; go test ./...")
	want := []string{"go vet ./...", "go test ./..."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestSplitCommandList_PreservesSmokeScript(t *testing.T) {
	smoke := `go build -o build/x . && (build/x & SRV=$!; sleep 1; curl -sf http://127.0.0.1:8080/health; RC=$?; kill $SRV; exit $RC)`
	got := splitCommandList(smoke)
	if len(got) != 1 || got[0] != smoke {
		t.Fatalf("smoke script must stay one command, got %#v", got)
	}
	// Mixing a simple command with a smoke script still splits the top-level ';'.
	mixed := `go build ./...; ` + smoke
	got = splitCommandList(mixed)
	if len(got) != 2 || got[0] != "go build ./..." || got[1] != smoke {
		t.Fatalf("mixed split = %#v", got)
	}
}

func TestSplitCommandList_RespectsQuotes(t *testing.T) {
	got := splitCommandList(`echo "a;b"; echo 'c;d'`)
	want := []string{`echo "a;b"`, `echo 'c;d'`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestSplitCommandList_EmptyAndWhitespace(t *testing.T) {
	if got := splitCommandList("  ; ;; "); len(got) != 0 {
		t.Fatalf("empty segments must be dropped, got %#v", got)
	}
}
