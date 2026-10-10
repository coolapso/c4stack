package scaffold

import (
	"errors"
	"go/format"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateValidatesBeforeCreatingAnything(t *testing.T) {
	root := t.TempDir()
	cases := map[string]Options{
		"bad name":        {Name: "Acme", Version: "v0.1.0"},
		"path name":       {Name: "../escape", Version: "v0.1.0"},
		"newline module":  {Name: "acme", Module: "example.com/acme\nreplace", Version: "v0.1.0"},
		"framework path":  {Name: "acme", Module: FrameworkModule, Version: "v0.1.0"},
		"latest":          {Name: "acme", Version: "latest"},
		"pseudo-version":  {Name: "acme", Version: "v0.1.1-0.20261010120000-abcdefabcdef"},
		"pre-release":     {Name: "acme", Version: "v0.1.0-rc.1"},
		"development":     {Name: "acme", Version: "(devel)"},
		"not a checkout":  {Name: "acme", Local: root},
		"missing version": {Name: "acme"},
	}
	for name, opts := range cases {
		opts.Dir = filepath.Join(root, "app")
		if err := Generate(opts); err == nil {
			t.Errorf("%s: Generate succeeded", name)
		}
		if _, err := os.Stat(opts.Dir); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s: created the directory before validating", name)
		}
	}
}

func TestGenerateNeverWritesIntoAnExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main // mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Generate(Options{Dir: dir, Name: "acme", Version: "v0.1.0"}); err == nil {
		t.Fatal("generated into an existing directory")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if string(got) != "package main // mine" {
		t.Fatal("an existing file was changed")
	}
	empty := t.TempDir()
	if err := Generate(Options{Dir: empty, Name: "acme", Version: "v0.1.0"}); err == nil {
		t.Fatal("generated into an existing empty directory")
	}
}

func generate(t *testing.T, opts Options) string {
	t.Helper()
	opts.Dir = filepath.Join(t.TempDir(), opts.Name)
	if err := Generate(opts); err != nil {
		t.Fatal(err)
	}
	return opts.Dir
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGeneratedApp(t *testing.T) {
	dir := generate(t, Options{Name: "order-desk", Module: "example.com/orders", Version: "v0.3.1"})

	for _, name := range []string{
		"main.go", "config.go", "routes.go", "layout.go", "pages.templ", "static/app.css", "static/favicon.svg",
		"migrations/migrations.go", "go.mod", "Taskfile.yml", ".taskrc.yml", ".env", ".env.example", ".gitignore",
		".dockerignore", "docker-compose.yml", "Containerfile", "README.md", "AGENTS.md",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s", name)
		}
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".go" {
			return err
		}
		src, _ := os.ReadFile(p)
		formatted, err := format.Source(src)
		if err != nil || string(formatted) != string(src) {
			t.Errorf("%s is not gofmt-formatted: %v", p, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	gomod := read(t, dir, "go.mod")
	if !strings.Contains(gomod, "module example.com/orders") || !strings.Contains(gomod, FrameworkModule+" v0.3.1") || strings.Contains(gomod, "replace") {
		t.Errorf("go.mod:\n%s", gomod)
	}
	taskfile := read(t, dir, "Taskfile.yml")
	if !strings.Contains(taskfile, "https://github.com/coolapso/c4stack.git//taskfiles/app.yml?ref={{.FRAMEWORK_VERSION}}") ||
		!strings.Contains(taskfile, "  FRAMEWORK_VERSION: v0.3.1\n") {
		t.Errorf("the Taskfile does not include the shared tasks at the pinned release:\n%s", taskfile)
	}
	if !strings.Contains(read(t, dir, "AGENTS.md"), "https://github.com/coolapso/c4stack/blob/v0.3.1/docs/guide.md") {
		t.Error("AGENTS.md does not link the guide of the pinned release")
	}
	if !strings.Contains(read(t, dir, "config.go"), `os.Getenv("ORDER_DESK_" + name)`) {
		t.Error("the environment prefix is not derived from the app name")
	}
	if !strings.Contains(read(t, dir, ".env"), "ORDER_DESK_SECRET_KEY=") || strings.Contains(read(t, dir, ".env"), "ORDER_DESK_SECRET_KEY=\n") {
		t.Error(".env lacks a generated secret key")
	}
	if info, _ := os.Stat(filepath.Join(dir, ".env")); info.Mode().Perm() != 0o600 {
		t.Errorf(".env mode %v", info.Mode().Perm())
	}
	if strings.Contains(read(t, dir, ".gitignore"), ".env.example") || !strings.Contains(read(t, dir, ".gitignore"), ".env") {
		t.Error(".gitignore must ignore .env")
	}

	// What users and operators see carries the app's name, never the framework's.
	// Import paths are code, not something users see.
	for _, name := range []string{"config.go", "routes.go", "layout.go", "pages.templ", ".env", ".env.example", "docker-compose.yml", "Containerfile", "static/app.css"} {
		for line := range strings.Lines(strings.ToLower(read(t, dir, name))) {
			if strings.Contains(line, `"github.com/coolapso/c4stack`) {
				continue
			}
			if strings.Contains(line, "c4stack") || strings.Contains(line, "c4_") {
				t.Errorf("%s mentions the framework: %s", name, line)
			}
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "migrations")); len(entries) != 1 {
		t.Error("the app should start without migrations")
	}
}

func TestLocalCheckout(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "c4stack")
	if err := os.MkdirAll(filepath.Join(checkout, "taskfiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module "+FrameworkModule+"\n\ngo 1.26.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "acme")
	if err := Generate(Options{Dir: dir, Name: "acme", Local: checkout}); err != nil {
		t.Fatal(err)
	}
	if gomod := read(t, dir, "go.mod"); !strings.Contains(gomod, "replace github.com/coolapso/c4stack => ../c4stack") {
		t.Errorf("go.mod:\n%s", gomod)
	}
	if !strings.Contains(read(t, dir, "Taskfile.yml"), "taskfile: ../c4stack/taskfiles/app.yml") {
		t.Error("a local app does not include the checkout's shared tasks")
	}
}

func TestIsRelease(t *testing.T) {
	for v, want := range map[string]bool{"v0.1.0": true, "v1.20.3": true, "v0.1": false, "0.1.0": false, "v0.1.0+dirty": false, "": false} {
		if IsRelease(v) != want {
			t.Errorf("IsRelease(%q) = %v", v, !want)
		}
	}
}
