//go:build linux

package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsolatedEnvDropsHostSecretsAndKeepsAllowlist(t *testing.T) {
	out := isolatedEnv([]string{
		"PATH=/usr/bin",
		"LANG=C",
		"DIVVY_AUDIT_HOST_SENTINEL=secret-value",
		"AWS_SECRET_ACCESS_KEY=ak",
		"GITHUB_TOKEN=gh",
		"HOME=/home/user",
		"GOMODCACHE=/host/cache",
	})
	joined := strings.Join(out, "\n")
	for _, leak := range []string{"DIVVY_AUDIT_HOST_SENTINEL", "secret-value", "AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN", "/home/user"} {
		if strings.Contains(joined, leak) {
			t.Fatalf("isolated env leaked %q: %s", leak, joined)
		}
	}
	hasPath, hasHomeTmp, hasProxyOff := false, false, false
	for _, kv := range out {
		if kv == "PATH=/usr/bin" || strings.HasPrefix(kv, "PATH=") {
			hasPath = true
		}
		if kv == "HOME=/tmp" {
			hasHomeTmp = true
		}
		if kv == "GOPROXY=off" {
			hasProxyOff = true
		}
	}
	if !hasPath || !hasHomeTmp || !hasProxyOff {
		t.Fatalf("allowlist/overrides missing: %v", out)
	}
}

func TestIsolatedGoModCacheAllowsCachedModuleAndHidesHostEnv(t *testing.T) {
	root := t.TempDir()
	ok, reason := ProbeShellIsolation(root)
	if !ok {
		t.Skip(reason)
	}
	if hostGoModCache() == "" {
		t.Fatal("host GOMODCACHE should be discoverable so it can be bound read-only")
	}
	mod := filepath.Join(hostGoModCache(), "github.com", "!burnt!sushi", "toml@v1.6.0")
	if _, err := os.Stat(mod); err != nil {
		t.Skip("cached github.com/BurntSushi/toml@v1.6.0 not present")
	}

	writeFile(t, filepath.Join(root, "go.mod"), "module demo\n\ngo 1.22\n\nrequire github.com/BurntSushi/toml v1.6.0\n")
	writeFile(t, filepath.Join(root, "main.go"), `package main
import (
	"fmt"
	"os"
	"github.com/BurntSushi/toml"
)
func main() {
	if v := os.Getenv("DIVVY_AUDIT_HOST_SENTINEL"); v != "" {
		fmt.Printf("LEAKED=%s\n", v)
		os.Exit(2)
	}
	var v struct{ X int }
	if err := toml.Unmarshal([]byte("X = 1\n"), &v); err != nil {
		panic(err)
	}
	fmt.Printf("ok home=%s proxy=%s cache=%s x=%d\n", os.Getenv("HOME"), os.Getenv("GOPROXY"), os.Getenv("GOMODCACHE"), v.X)
}
`)
	// Populate go.sum from the host cache only; the isolated run uses GOPROXY=off.
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = root
	tidy.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off")
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("host go mod tidy (cache only): %v\n%s", err, out)
	}

	cmd, err := isolateShellCommand(context.Background(), root, "go run .", append(os.Environ(), "DIVVY_AUDIT_HOST_SENTINEL=secret-value"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated go run: %v\n%s", err, out)
	}
	got := string(out)
	if strings.Contains(got, "LEAKED=") || strings.Contains(got, "secret-value") {
		t.Fatalf("host sentinel leaked into isolate: %s", got)
	}
	if !strings.Contains(got, "ok home=/tmp proxy=off cache=/tmp/divvy-gomodcache x=1") {
		t.Fatalf("unexpected isolated go run output: %s", got)
	}
}
