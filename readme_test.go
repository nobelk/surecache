// SPDX-License-Identifier: Apache-2.0

package surecache_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Compile the actual README quickstart against the external example's store, so
// API drift (including store result arity) cannot hide in documentation.
func TestREADMEQuickstartCompiles(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(readme), "## Quick start\n")
	if !ok {
		t.Fatal("README quickstart heading missing")
	}
	_, code, ok := strings.Cut(section, "```go\n")
	if !ok {
		t.Fatal("README quickstart Go block missing")
	}
	code, _, ok = strings.Cut(code, "\n```")
	if !ok {
		t.Fatal("README quickstart Go block is not closed")
	}

	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	example, err := os.ReadFile("example_test.go")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod":          fmt.Sprintf("module readmecheck\n\ngo 1.25.0\n\nrequire github.com/nobelk/surecache v0.0.0\nreplace github.com/nobelk/surecache => %q\n", root),
		"example_test.go": string(example),
		"quickstart_test.go": `package surecache_test
import (
    "context"
    "errors"
    "fmt"
    "slices"
    "strings"
    "github.com/nobelk/surecache"
)
func readmeQuickstart(ctx context.Context, store *store) error {
` + code + "\nreturn nil\n}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-mod=readonly", "-run=^$", "-timeout=30s", ".")
	// A compiler descendant may inherit the output pipes after the go process
	// is killed; bound CombinedOutput's pipe drain as well as the process wait.
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("README quickstart does not compile: %v\n%s", err, out)
	}
}
