package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseExampleGoPostgresRedis(t *testing.T) {
	cfg, err := ParseFile(filepath.Join("..", "..", "examples", "go-postgres-redis", "Yakfile"), "")
	if err != nil {
		t.Fatalf("ParseFile failed:\n%v", err)
	}
	if cfg.Project.Name != "shop" || cfg.Project.FreeBSD != "14.2-RELEASE" {
		t.Fatalf("unexpected project: %+v", cfg.Project)
	}
	if cfg.SelectedTarget == nil || cfg.SelectedTarget.Host != "deploy@203.0.113.10" {
		t.Fatalf("unexpected selected target: %+v", cfg.SelectedTarget)
	}
	if len(cfg.Storage) != 3 {
		t.Fatalf("expected 3 storage blocks (uploads + pg_data + redis_data), got %d", len(cfg.Storage))
	}
	if len(cfg.Services) != 4 {
		t.Fatalf("expected 4 services (db, cache, api, worker), got %d", len(cfg.Services))
	}
	if len(cfg.ReferencedSecrets) != 2 || cfg.ReferencedSecrets[0] != "jwt_secret" || cfg.ReferencedSecrets[1] != "postgres_password" {
		t.Fatalf("unexpected referenced secrets: %v", cfg.ReferencedSecrets)
	}

	// Verify topological order puts db and cache before api and worker.
	idx := map[string]int{}
	for i, name := range cfg.ServiceOrder {
		idx[name] = i
	}
	if idx["db"] > idx["api"] || idx["cache"] > idx["api"] || idx["db"] > idx["worker"] {
		t.Fatalf("invalid topological service order: %v", cfg.ServiceOrder)
	}
	if cfg.Services["api"].Strategy != "blue-green" || cfg.Services["worker"].Strategy != "recreate" {
		t.Fatalf("unexpected strategies: api=%s worker=%s", cfg.Services["api"].Strategy, cfg.Services["worker"].Strategy)
	}
}

func TestParseExampleGoLocalBuild(t *testing.T) {
	cfg, err := ParseFile(filepath.Join("..", "..", "examples", "go-local-build", "Yakfile"), "")
	if err != nil {
		t.Fatalf("ParseFile local build failed: %v", err)
	}
	if cfg.Images["app"].Stages[0].On != "local" {
		t.Fatalf("expected local stage, got %s", cfg.Images["app"].Stages[0].On)
	}
}

func TestDetectDependencyCycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Yakfile")
	src := `
project "cycle" {
  freebsd = "14.2-RELEASE"
}
image "app" {
  runtime {}
}
service "a" {
  image      = image.app
  depends_on = [service.b]
  cmd        = "/bin/a"
}
service "b" {
  image      = image.app
  depends_on = [service.a]
  cmd        = "/bin/b"
}
`
	_ = os.WriteFile(path, []byte(src), 0o644)
	_, err := ParseFile(path, "")
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected dependency cycle error, got: %v", err)
	}
}

func TestDetectIncludeLoopAndTraversal(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "Yakfile")
	b := filepath.Join(dir, "b.hcl")
	_ = os.WriteFile(a, []byte(`include = ["b.hcl"]`+"\n"+`project "p" { freebsd = "14.2-RELEASE" }`), 0o644)
	_ = os.WriteFile(b, []byte(`include = ["Yakfile"]`), 0o644)

	_, err := ParseFile(a, "")
	if err == nil || !strings.Contains(err.Error(), "Circular include") {
		t.Fatalf("expected circular include error, got: %v", err)
	}

	_ = os.WriteFile(a, []byte(`include = ["../outside.hcl"]`), 0o644)
	_, err = ParseFile(a, "")
	if err == nil || !strings.Contains(err.Error(), "escapes project root") {
		t.Fatalf("expected traversal error, got: %v", err)
	}
}

func TestTargetVarsAndUndefinedRef(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Yakfile")
	src := `
project "vars" {
  freebsd = "14.2-RELEASE"
}
target "staging" {
  host = "deploy@10.0.0.2"
  vars = { greeting = "hi-staging" }
}
image "app" {
  runtime {}
}
service "web" {
  image = image.app
  env   = { MSG = var.greeting }
  cmd   = "/bin/web"
}
`
	_ = os.WriteFile(path, []byte(src), 0o644)
	cfg, err := ParseFile(path, "staging")
	if err != nil {
		t.Fatalf("ParseFile failed: %v", err)
	}
	if cfg.Services["web"].Env["MSG"] != "hi-staging" {
		t.Fatalf("expected hi-staging, got %q", cfg.Services["web"].Env["MSG"])
	}

	// Undefined reference should fail cleanly with HCL diagnostic.
	bad := `
project "bad" {
  freebsd = "14.2-RELEASE"
}
image "app" {
  runtime {}
}
service "web" {
  image = image.missing
  cmd   = "/bin/web"
}
`
	_ = os.WriteFile(path, []byte(bad), 0o644)
	if _, err := ParseFile(path, ""); err == nil {
		t.Fatal("expected error for undefined image.missing")
	}
}
