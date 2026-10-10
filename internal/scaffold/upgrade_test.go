//go:build upgrade

package scaffold

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"github.com/coolapso/c4stack/internal/testdb"
)

// TestUpgrade moves a generated app from release N to N+1 with task framework:update.
// The releases are this checkout's tracked files, and the same files with a new
// migration, a new operator command, a fixed default page and a new shared task. The
// app must pick all of them up while its own files and data stay as they were.
//
// It needs Go, Git, Task and TEST_DATABASE_URL; task test:upgrade provides them. Go
// commands use a private module cache, so the test releases never reach the real one.
func TestUpgrade(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	checkout := filepath.Join(filepath.Dir(file), "..", "..")
	work := t.TempDir()

	n, next := filepath.Join(work, "n"), filepath.Join(work, "next")
	copyTracked(t, checkout, n)
	copyTracked(t, checkout, next)
	changeRelease(t, next)

	proxy := filepath.Join(work, "proxy")
	publish(t, proxy, n, "v0.1.0")
	publish(t, proxy, next, "v0.1.1")
	git := serveTaskfiles(t, work, n, next)

	realCache := goEnv(t, "GOMODCACHE")
	env := append(os.Environ(),
		"GOMODCACHE="+filepath.Join(work, "modcache"),
		"GOCACHE="+goEnv(t, "GOCACHE"),
		"GOPROXY=file://"+proxy+",file://"+filepath.Join(realCache, "cache", "download")+",https://proxy.golang.org",
		"GOPRIVATE=", "GONOPROXY=", "GOSUMDB=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local",
		"XDG_CACHE_HOME="+filepath.Join(work, "cache"),
	)
	t.Cleanup(func() {
		cmd := exec.Command("go", "clean", "-modcache")
		cmd.Env = env
		cmd.Run()
	})

	dir := filepath.Join(work, "acme")
	if err := Generate(Options{Dir: dir, Name: "acme", Version: "v0.1.0"}); err != nil {
		t.Fatal(err)
	}
	// Point the app at the local Git server, as it would point at GitHub.
	host := strings.TrimPrefix(git.URL, "http://")
	edit(t, filepath.Join(dir, "Taskfile.yml"), "https://github.com/coolapso/c4stack.git", git.URL+"/c4stack.git")
	write(t, filepath.Join(dir, ".taskrc.yml"), "remote:\n  insecure: true\n  trusted-hosts:\n    - "+host+"\n")
	// The app's own work: a table that references accounts.
	write(t, filepath.Join(dir, "migrations", "001_notes.sql"),
		"CREATE TABLE notes (account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE, body text NOT NULL);\n")

	run := func(extra []string, name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(env, extra...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	run(nil, "go", "mod", "tidy")
	run(nil, "go", "tool", "templ", "generate")
	run(nil, "go", "mod", "tidy")
	run(nil, "task", "build")
	if strings.Contains(run(nil, "task", "--list"), "probe") {
		t.Fatal("release N already has the new shared task")
	}

	databaseURL := testdb.New(t)
	dbEnv := []string{"ACME_DATABASE_URL=" + databaseURL}
	run(dbEnv, "task", "db:migrate")
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var account string
	if err := db.QueryRowContext(ctx, `INSERT INTO accounts (provider, subject) VALUES ('hanko', 'ada') RETURNING id`).Scan(&account); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO notes VALUES ($1, 'kept across the upgrade')`, account); err != nil {
		t.Fatal(err)
	}
	before := appFiles(t, dir)
	taskfile := readFile(t, filepath.Join(dir, "Taskfile.yml"))

	run(nil, "task", "framework:update", "VERSION=v0.1.1")

	if gomod := readFile(t, filepath.Join(dir, "go.mod")); !strings.Contains(gomod, "github.com/coolapso/c4stack v0.1.1") {
		t.Fatalf("go.mod after the update:\n%s", gomod)
	}
	if out := run(nil, "task", "probe"); !strings.Contains(out, "shared probe from the new release") {
		t.Fatalf("the new shared task: %s", out)
	}
	run(dbEnv, "task", "db:migrate")
	var probe, notes int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM upgrade_probe`).Scan(&probe); err != nil {
		t.Fatalf("the new release's migration did not run: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM notes WHERE account_id = $1`, account).Scan(&notes); err != nil || notes != 1 {
		t.Fatalf("app data after the upgrade: %d %v", notes, err)
	}

	appEnv := []string{"ACME_PUBLIC_URL=http://localhost:8080", "ACME_SECRET_KEY=" + strings.Repeat("k", 32), "ACME_HANKO_API_URL=https://tenant.hanko.io"}
	binary := filepath.Join(dir, "bin", "acme")
	if out := run(append(dbEnv, appEnv...), binary, "accounts", "probe"); !strings.Contains(out, "probe from the new release") {
		t.Fatalf("the new operator command: %s", out)
	}
	if out := run(nil, binary, "version"); !strings.Contains(out, "github.com/coolapso/c4stack v0.1.1") {
		t.Fatalf("version: %s", out)
	}
	if built, _ := os.ReadFile(binary); !bytes.Contains(built, []byte("Permanently remove your account and all of its data.")) {
		t.Fatal("the fixed default page did not reach the binary")
	}

	// The update changes go.mod, go.sum and the version line in Taskfile.yml, nothing else.
	after := appFiles(t, dir)
	for name, sum := range before {
		if after[name] != sum && name != "Taskfile.yml" {
			t.Errorf("the update changed the app's %s", name)
		}
	}
	if got, want := readFile(t, filepath.Join(dir, "Taskfile.yml")), strings.Replace(taskfile, "FRAMEWORK_VERSION: v0.1.0", "FRAMEWORK_VERSION: v0.1.1", 1); got != want {
		t.Errorf("Taskfile.yml after the update:\n%s", got)
	}

	// The shared check catches a Taskfile that no longer matches go.mod.
	write(t, filepath.Join(dir, "Taskfile.yml"), taskfile)
	cmd := exec.Command("task", "check")
	cmd.Dir, cmd.Env = dir, env
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "task framework:update VERSION=v0.1.1") {
		t.Fatalf("check with a stale Taskfile: %v\n%s", err, out)
	}
}

// changeRelease turns a copy of the checkout into the next release.
func changeRelease(t *testing.T, dir string) {
	t.Helper()
	write(t, filepath.Join(dir, "accounts", "migrations", "002_upgrade_probe.sql"), "CREATE TABLE upgrade_probe (id integer);\n")
	write(t, filepath.Join(dir, "accounts", "probe.go"), `package accounts

import (
	"context"
	"fmt"
)

func probe(context.Context, []string) error {
	fmt.Println("probe from the new release")
	return nil
}
`)
	edit(t, filepath.Join(dir, "accounts", "accounts.go"),
		"\ts := &Accounts{app: a, idp: idp, cfg: cfg}\n",
		"\ts := &Accounts{app: a, idp: idp, cfg: cfg}\n\ta.Command(app.Command{Name: \"accounts probe\", Run: probe})\n")
	for _, name := range []string{"pages.templ", "pages_templ.go"} {
		edit(t, filepath.Join(dir, "accounts", name),
			"Permanently remove your account and its data.", "Permanently remove your account and all of its data.")
	}
	edit(t, filepath.Join(dir, "taskfiles", "app.yml"), "\ntasks:\n",
		"\ntasks:\n  probe:\n    desc: A task the next release adds\n    cmds:\n      - echo shared probe from the new release\n\n")
}

// copyTracked copies the files Git tracks, so a file someone forgot to add breaks the
// release here too.
func copyTracked(t *testing.T, from, to string) {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = from
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for name := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		data, err := os.ReadFile(filepath.Join(from, name))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(to, name), string(data))
	}
}

// publish adds a release of the module in dir to a file-based module proxy.
func publish(t *testing.T, proxy, dir, version string) {
	t.Helper()
	at := filepath.Join(proxy, FrameworkModule, "@v")
	if err := os.MkdirAll(at, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(at, version+".zip"))
	if err != nil {
		t.Fatal(err)
	}
	if err := modzip.CreateFromDir(f, module.Version{Path: FrameworkModule, Version: version}, dir); err != nil {
		t.Fatal(err)
	}
	f.Close()
	write(t, filepath.Join(at, version+".mod"), readFile(t, filepath.Join(dir, "go.mod")))
	write(t, filepath.Join(at, version+".info"), fmt.Sprintf(`{"Version":%q,"Time":%q}`, version, time.Now().UTC().Format(time.RFC3339)))
	list, _ := os.ReadFile(filepath.Join(at, "list"))
	write(t, filepath.Join(at, "list"), string(list)+version+"\n")
}

// serveTaskfiles serves a Git repository with both releases tagged, over smart HTTP.
func serveTaskfiles(t *testing.T, work, n, next string) *httptest.Server {
	t.Helper()
	repo := filepath.Join(work, "repo")
	gitIn := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	for i, release := range []struct{ dir, tag string }{{n, "v0.1.0"}, {next, "v0.1.1"}} {
		write(t, filepath.Join(repo, "taskfiles", "app.yml"), readFile(t, filepath.Join(release.dir, "taskfiles", "app.yml")))
		if i == 0 {
			gitIn("init", "-q")
		}
		gitIn("add", ".")
		gitIn("commit", "-q", "-m", release.tag)
		gitIn("tag", release.tag)
	}
	root := filepath.Join(work, "served")
	cmd := exec.Command("git", "clone", "-q", "--bare", repo, filepath.Join(root, "c4stack.git"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&cgi.Handler{
		Path: gitPath,
		Root: "/",
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	})
	t.Cleanup(srv.Close)
	return srv
}

// appFiles hashes the app's own files: everything but go.mod, go.sum and build output.
func appFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	sums := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			if rel == "bin" || rel == ".task" {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == "go.mod" || rel == "go.sum" {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		sums[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sums
}

func goEnv(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("go", "env", name).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func edit(t *testing.T, path, old, replacement string) {
	t.Helper()
	content := readFile(t, path)
	if !strings.Contains(content, old) {
		t.Fatalf("%s does not contain %q", path, old)
	}
	write(t, path, strings.Replace(content, old, replacement, 1))
}
