//go:build freebsd_e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFreeBSDHostLifecycle(t *testing.T) {
	host := os.Getenv("YAK_E2E_HOST")
	if host == "" {
		t.Skip("set YAK_E2E_HOST (or run `make test-e2e-vm`) to execute FreeBSD kernel E2E tests")
	}

	yakBin := os.Getenv("YAK_BIN")
	if yakBin == "" {
		yakBin = "yak"
	}

	dir := t.TempDir()
	mainDir := filepath.Join(dir, "cmd", "hello")
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		t.Fatal(err)
	}

	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/e2ehello\n\ngo 1.24\n"), 0o644)
	_ = os.WriteFile(filepath.Join(mainDir, "main.go"), []byte(`package main
import (
	"fmt"
	"net/http"
	"os"
)
func main() {
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello:%s\n", os.Getenv("GREETING"))
	})
	_ = http.ListenAndServe(":8080", nil)
}
`), 0o644)

	freebsdVer := os.Getenv("YAK_E2E_FREEBSD_VERSION")
	if freebsdVer == "" {
		freebsdVer = "14.5-RELEASE"
	}

	yakfile := fmt.Sprintf(`project "e2e" {
  freebsd = %q
}

target "vm" {
  host    = %q
  default = true
}

image "app" {
  stage "go" {
    on = "local"
    env = {
      CGO_ENABLED = "0"
      GOOS        = "freebsd"
      GOARCH      = "amd64"
    }
    steps = ["go build -o ./dist/hello ./cmd/hello"]
  }

  runtime {
    packages = ["ca_root_nss"]
    copy {
      from = stage.go
      src  = "./dist/hello"
      dst  = "/app/hello"
    }
    workdir = "/app"
  }
}

service "web" {
  image    = image.app
  strategy = "blue-green"
  env = {
    GREETING = secret.greeting
  }
  cmd  = "/app/hello"
  port = 8080
  healthcheck {
    http     = "/healthz"
    interval = "1s"
    timeout  = "20s"
  }
  expose {
    listen = "127.0.0.1:18099"
    to     = 8080
  }
}
`, freebsdVer, host)

	yakfilePath := filepath.Join(dir, "Yakfile")
	if err := os.WriteFile(yakfilePath, []byte(yakfile), 0o644); err != nil {
		t.Fatal(err)
	}

	var sshCfg string
	if sshKey := os.Getenv("YAK_E2E_SSH_KEY"); sshKey != "" {
		sshCfg = filepath.Join(dir, "ssh_config")
		_ = os.WriteFile(sshCfg, []byte("Host *\n  IdentityFile "+sshKey+"\n  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n  LogLevel ERROR\n"), 0o600)
	}

	runYak := func(args ...string) string {
		t.Helper()
		full := append([]string{"-F", yakfilePath}, args...)
		cmd := exec.Command(yakBin, full...)
		cmd.Dir = dir
		if sshCfg != "" {
			cmd.Env = append(os.Environ(), "YAK_SSH_CONFIG="+sshCfg)
		}
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("yak %s failed: %v\n%s", strings.Join(full, " "), err, out.String())
		}
		return out.String()
	}

	runSSH := func(remoteCmd string) string {
		t.Helper()
		sshHost := host
		var sshArgs []string
		if sshCfg != "" {
			sshArgs = append(sshArgs, "-F", sshCfg)
		}
		if idx := strings.LastIndex(host, ":"); idx != -1 && !strings.Contains(host, "[") {
			sshHost = host[:idx]
			sshArgs = append(sshArgs, "-p", host[idx+1:])
		}
		sshArgs = append(sshArgs, sshHost, remoteCmd)
		cmd := exec.Command("ssh", sshArgs...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("ssh %q failed: %v\nstdout: %s\nstderr: %s", remoteCmd, err, stdout.String(), stderr.String())
		}
		return stdout.String()
	}

	defer func() {
		downCmd := exec.Command(yakBin, "-F", yakfilePath, "down")
		downCmd.Dir = dir
		if sshCfg != "" {
			downCmd.Env = append(os.Environ(), "YAK_SSH_CONFIG="+sshCfg)
		}
		_ = downCmd.Run()
	}()

	runYak("secret", "set", "greeting", "v1")
	runYak("deploy", "--dry-run")
	runYak("deploy")

	psOut := runYak("ps")
	if !strings.Contains(psOut, "web") || !strings.Contains(psOut, "running") {
		t.Fatalf("unexpected ps output:\n%s", psOut)
	}

	ipOut := strings.TrimSpace(runYak("ps", "web", "--ip"))
	if !strings.HasPrefix(ipOut, "10.88.") {
		t.Fatalf("expected VNET IP 10.88.x.x, got %q", ipOut)
	}

	if body := strings.TrimSpace(runSSH("fetch -qo - http://127.0.0.1:18099/")); body != "hello:v1" {
		t.Fatalf("expected hello:v1 from exposed endpoint, got %q", body)
	}

	execOut := runYak("exec", "web", "--", "uname", "-s")
	if strings.TrimSpace(execOut) != "FreeBSD" {
		t.Fatalf("expected FreeBSD inside jail, got %q", execOut)
	}

	logsOut := runYak("logs", "web", "-n", "20")
	if strings.TrimSpace(logsOut) == "" {
		t.Fatalf("expected non-empty service logs")
	}

	// Deploy v2 and verify live cutover + rollback to r1.
	runYak("secret", "set", "greeting", "v2")
	runYak("deploy")
	if body := strings.TrimSpace(runSSH("fetch -qo - http://127.0.0.1:18099/")); body != "hello:v2" {
		t.Fatalf("expected hello:v2 after blue/green cutover, got %q", body)
	}

	runYak("deploy", "web", "--release", "r1")
	runYak("down")
}
