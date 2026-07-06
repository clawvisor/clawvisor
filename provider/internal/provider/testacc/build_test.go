package testacc

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

var (
	buildOnce sync.Once
	buildBin  string
	buildErr  error
)

// buildServerBinary compiles cmd/clawvisor-server once per test process and
// returns the path. Mirrors e2e/testapp/build.go but without a *testing.T.
func buildServerBinary() (string, error) {
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "clawvisor-testacc-build-")
		if err != nil {
			buildErr = err
			return
		}
		bin := filepath.Join(dir, "clawvisor-server")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", bin, "github.com/clawvisor/clawvisor/cmd/clawvisor-server")
		cmd.Env = os.Environ()
		// Build in the parent (OSS server) module's context. The provider
		// module only `replace`s the parent — it does not require it — so
		// building from the provider dir fails ("replaced but not required").
		cmd.Dir = repoRoot()
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build clawvisor-server: %w\n%s", err, out)
			return
		}
		buildBin = bin
	})
	return buildBin, buildErr
}

// repoRoot returns the parent (OSS server) module root, computed from this
// source file's location:
// <root>/provider/internal/provider/testacc/build_test.go.
func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
