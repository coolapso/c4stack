// Package scaffold generates a new C4Stack app. It only ever creates a new directory;
// it never writes into an existing one.
package scaffold

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"go/format"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"golang.org/x/mod/module"
)

// FrameworkModule is the module path apps require.
const FrameworkModule = "github.com/coolapso/c4stack"

// GoVersion is the go directive generated apps start with.
const GoVersion = "1.26.0"

//go:embed all:templates
var templates embed.FS

// Options describe the app to generate.
type Options struct {
	// Dir is the directory to create. It must not exist.
	Dir string
	// Name is the app's name: lowercase letters, digits and hyphens, starting with a
	// letter. It names the binary and derives the environment variable prefix.
	Name string
	// Module is the Go module path. Zero means Name.
	Module string
	// Version is the C4Stack release to pin, such as v0.1.0. It is ignored with Local.
	Version string
	// Local is the path of a C4Stack checkout to build against during development. The
	// app then gets a replace directive and includes the checkout's shared Taskfile.
	Local string
}

var (
	namePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	releasePattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
)

// IsRelease reports whether v names a release, as opposed to a pseudo-version, a
// pre-release or a development build.
func IsRelease(v string) bool {
	return releasePattern.MatchString(v)
}

// data is what the templates see.
type data struct {
	Name      string
	Title     string
	Module    string
	EnvPrefix string
	Version   string
	Local     string // replace target, relative to the app
	Taskfile  string // the shared Taskfile include
	GoVersion string
	SecretKey string
}

// Generate creates opts.Dir with a new app. It validates everything before creating
// anything, and fails if the directory exists.
func Generate(opts Options) error {
	d, err := prepare(opts)
	if err != nil {
		return err
	}
	if err := os.Mkdir(opts.Dir, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists; the generator only creates new directories", opts.Dir)
		}
		return err
	}
	return fs.WalkDir(templates, "templates", func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(p, "templates/"), ".tmpl")
		raw, err := fs.ReadFile(templates, p)
		if err != nil {
			return err
		}
		out := raw
		if strings.HasSuffix(p, ".tmpl") {
			if out, err = render(rel, raw, d); err != nil {
				return err
			}
		}
		target := filepath.Join(opts.Dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if rel == ".env" {
			mode = 0o600
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		if _, err := f.Write(out); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	})
}

func prepare(opts Options) (data, error) {
	if opts.Dir == "" {
		return data{}, errors.New("a directory is required")
	}
	if !namePattern.MatchString(opts.Name) {
		return data{}, fmt.Errorf("invalid app name %q: use lowercase letters, digits and hyphens, starting with a letter", opts.Name)
	}
	mod := opts.Module
	if mod == "" {
		mod = opts.Name
	}
	if err := module.CheckImportPath(mod); err != nil {
		return data{}, fmt.Errorf("invalid module path %q: %w", mod, err)
	}
	if mod == FrameworkModule || strings.HasPrefix(mod, FrameworkModule+"/") {
		return data{}, errors.New("the app's module path cannot be C4Stack's own")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return data{}, err
	}
	d := data{
		Name:      opts.Name,
		Title:     title(opts.Name),
		Module:    mod,
		EnvPrefix: strings.ToUpper(strings.ReplaceAll(opts.Name, "-", "_")),
		GoVersion: GoVersion,
		SecretKey: hex.EncodeToString(key),
	}
	if opts.Local != "" {
		local, err := localCheckout(opts.Local, opts.Dir)
		if err != nil {
			return data{}, err
		}
		d.Version = "v0.0.0"
		d.Local = local
		d.Taskfile = path.Join(local, "taskfiles/app.yml")
		return d, nil
	}
	if !IsRelease(opts.Version) {
		return data{}, fmt.Errorf("version %q is not a release such as v0.1.0; pass --version, or --local for development", opts.Version)
	}
	d.Version = opts.Version
	d.Taskfile = "https://" + FrameworkModule + ".git//taskfiles/app.yml?ref={{.FRAMEWORK_VERSION}}"
	return d, nil
}

// localCheckout checks that dir is a C4Stack checkout and returns its path relative to
// the app directory.
func localCheckout(dir, appDir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if abs, err = filepath.EvalSymlinks(abs); err != nil {
		return "", fmt.Errorf("local checkout: %w", err)
	}
	mod, err := os.ReadFile(filepath.Join(abs, "go.mod"))
	if err != nil || !strings.HasPrefix(string(mod), "module "+FrameworkModule+"\n") {
		return "", fmt.Errorf("%s is not a C4Stack checkout", dir)
	}
	appAbs, err := filepath.Abs(appDir)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(appAbs))
	if err != nil {
		return "", err
	}
	// A nearby checkout, such as a sibling directory, is written relative to the app;
	// anything further away is written as an absolute path.
	rel, err := filepath.Rel(filepath.Join(parent, filepath.Base(appAbs)), abs)
	rel = filepath.ToSlash(rel)
	if err != nil || strings.Count(rel, "../") > 2 {
		return filepath.ToSlash(abs), nil //nolint:nilerr // an absolute path always works
	}
	if rel != ".." && !strings.HasPrefix(rel, "../") {
		rel = "./" + rel
	}
	return rel, nil
}

func title(name string) string {
	words := strings.Split(name, "-")
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

func render(name string, raw []byte, d data) ([]byte, error) {
	t, err := template.New(name).Delims("[[", "]]").Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return nil, fmt.Errorf("template %s: %w", name, err)
	}
	if strings.HasSuffix(name, ".go") {
		formatted, err := format.Source(buf.Bytes())
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		return formatted, nil
	}
	return buf.Bytes(), nil
}
