package tools

import (
	"strings"
	"testing"
)

func TestCompactDiagnostics_CMakeCompilerAndCTest(t *testing.T) {
	raw := `Scanning dependencies of target app
[ 50%] Building CXX object CMakeFiles/app.dir/main.cpp.o
src/main.cpp:12:3: error: 'foo' was not declared in this scope
CMake Error at CMakeLists.txt:10 (message):
  missing dependency
1: Test #1: server_smoke ................***Failed    0.12 sec
Errors while running CTest
`
	got := CompactDiagnostics(ProjectCMake, raw)
	if got == "" {
		t.Fatal("cmake failures must produce a compact block")
	}
	for _, want := range []string{
		"src/main.cpp:12:3: error:",
		"CMake Error at CMakeLists.txt:10",
		"***Failed",
		"Errors while running CTest",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Scanning dependencies") || strings.Contains(got, "Building CXX") {
		t.Fatalf("progress noise must be filtered:\n%s", got)
	}
}

func TestCompactDiagnostics_CMakeEmptyWithoutRulesWasBug(t *testing.T) {
	// Regression: before ProjectCMake diagRules existed, CompactDiagnostics
	// returned "" for cmake workspaces even when the log had root causes.
	raw := "src/a.cpp:1:1: error: broken\n"
	if got := CompactDiagnostics(ProjectCMake, raw); got == "" {
		t.Fatal("ProjectCMake must have diagRules so retries get compressed causes")
	}
}
