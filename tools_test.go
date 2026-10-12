package buildtools_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTools_ReportPackageErrors(t *testing.T) {
	for _, tool := range []struct {
		name string
		args []string
	}{
		{"gen-starlark-bindings", []string{"go", "-"}},
		{"typecheck-rpc", nil},
	} {
		t.Run(tool.name, func(t *testing.T) {
			dir := t.TempDir()
			binary := filepath.Join(dir, "tool.exe")
			cmd := exec.Command("go", "build", "-o", binary, "./cmd/"+tool.name)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build tool: %v\n%s", err, out)
			}

			rpcDir := filepath.Join(dir, "service", "rpc2")
			if err := os.MkdirAll(rpcDir, 0o755); err != nil {
				t.Fatal(err)
			}
			for path, contents := range map[string]string{
				filepath.Join(dir, "go.mod"): "module github.com/go-delve/delve\n\ngo 1.25.0\n",
				filepath.Join(rpcDir, "client.go"): `package rpc2

type RPCServer struct{}
type RPCClient struct{}

func (*RPCServer) State(args MissingArgs, reply *int) error { return nil }
`,
			} {
				if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			cmd = exec.Command(binary, tool.args...)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("tool succeeded with an invalid package:\n%s", out)
			}
			if !strings.Contains(string(out), "undefined: MissingArgs") {
				t.Errorf("missing package error:\n%s", out)
			}
			if strings.Contains(string(out), "panic:") {
				t.Errorf("tool panicked instead of reporting the package error:\n%s", out)
			}
		})
	}
}
