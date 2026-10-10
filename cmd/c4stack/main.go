// Command c4stack generates new C4Stack apps.
//
//	c4stack new <directory> [--name NAME] [--module PATH] [--version vX.Y.Z | --local PATH]
//
// It creates the directory, which must not exist, writes the app's starting files, and
// runs go mod tidy and templ generate in it.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"

	"github.com/coolapso/c4stack/internal/scaffold"
)

// minTask is the first Task release with remote Taskfiles.
var minTask = [3]int{3, 51, 1}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(stdout)
		return 0
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, buildVersion())
		return 0
	case "new":
		return newApp(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "c4stack: unknown command %q\n", args[0])
	usage(stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  c4stack new <directory> [flags]   Generate a new app in a new directory
  c4stack version                   Show this generator's C4Stack release

Flags for new:
  --name NAME       App name (default: the directory's name)
  --module PATH     Go module path (default: the app name)
  --version vX.Y.Z  C4Stack release to pin (default: this generator's release)
  --local PATH      Build against a local C4Stack checkout (development only)
  --skip-setup      Do not run go mod tidy and templ generate
`)
}

func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Main.Version
	}
	return "(unknown)"
}

func newApp(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "")
	mod := fs.String("module", "", "")
	version := fs.String("version", "", "")
	local := fs.String("local", "", "")
	skipSetup := fs.Bool("skip-setup", false, "")
	// Accept flags before or after the directory.
	var dir string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return 2
		}
		args = fs.Args()
		if len(args) > 0 {
			if dir != "" {
				fmt.Fprintln(stderr, "c4stack: new takes one directory")
				return 2
			}
			dir, args = args[0], args[1:]
		}
	}
	if dir == "" {
		fmt.Fprintln(stderr, "c4stack: new needs a directory")
		return 2
	}
	if *name == "" {
		*name = filepath.Base(filepath.Clean(dir))
	}
	if *version == "" && *local == "" {
		*version = buildVersion()
	}

	if err := scaffold.Generate(scaffold.Options{Dir: dir, Name: *name, Module: *mod, Version: *version, Local: *local}); err != nil {
		fmt.Fprintf(stderr, "c4stack: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Created %s.\n", dir)
	warnOldTask(stderr)
	if !*skipSetup {
		// Tidy again after generating: the generated code imports templ directly.
		for _, step := range [][]string{{"go", "mod", "tidy"}, {"go", "tool", "templ", "generate"}, {"go", "mod", "tidy"}} {
			cmd := exec.Command(step[0], step[1:]...)
			cmd.Dir, cmd.Stdout, cmd.Stderr = dir, stderr, stderr
			if err := cmd.Run(); err != nil {
				fmt.Fprintf(stderr, "c4stack: %v failed: %v\nFinish the setup in %s with: go mod tidy && go tool templ generate\n", step, err, dir)
				return 1
			}
		}
	}
	fmt.Fprintf(stdout, `
Next steps:
  cd %s
  # Put your Hanko tenant's API URL in .env, then:
  task db:up db:migrate
  task dev
`, dir)
	return 0
}

var taskVersion = regexp.MustCompile(`([0-9]+)\.([0-9]+)\.([0-9]+)`)

// warnOldTask warns when Task is missing or too old for remote Taskfiles.
func warnOldTask(w io.Writer) {
	out, err := exec.Command("task", "--version").Output()
	if err != nil {
		fmt.Fprintf(w, "Note: install Task %d.%d.%d or later (https://taskfile.dev) to use the app's tasks.\n", minTask[0], minTask[1], minTask[2])
		return
	}
	m := taskVersion.FindStringSubmatch(string(out))
	if m == nil {
		return
	}
	for i := range 3 {
		n, _ := strconv.Atoi(m[i+1])
		if n != minTask[i] {
			if n < minTask[i] {
				fmt.Fprintf(w, "Note: Task %s is older than %d.%d.%d, which the app's Taskfile needs.\n", m[0], minTask[0], minTask[1], minTask[2])
			}
			return
		}
	}
}
