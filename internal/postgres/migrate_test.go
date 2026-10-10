package postgres

import (
	"strings"
	"testing"
	"testing/fstest"
)

func files(names ...string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for _, n := range names {
		fsys[n] = &fstest.MapFile{Data: []byte("SELECT 1; -- " + n)}
	}
	return fsys
}

func TestLoadRejectsAmbiguousHistories(t *testing.T) {
	bad := map[string][]Set{
		"bad namespace":     {{Namespace: "App", Files: files("001_a.sql")}},
		"duplicate set":     {{Namespace: "app", Files: files()}, {Namespace: "app", Files: files()}},
		"duplicate version": {{Namespace: "app", Files: files("001_a.sql", "1_b.sql")}},
		"zero version":      {{Namespace: "app", Files: files("000_a.sql")}},
		"bad name":          {{Namespace: "app", Files: files("first.sql")}},
		"upper case name":   {{Namespace: "app", Files: files("001_Init.sql")}},
		"no files":          {{Namespace: "app"}},
	}
	for name, sets := range bad {
		if _, err := load(sets); err == nil {
			t.Errorf("%s: load succeeded", name)
		}
	}
	got, err := load([]Set{{Namespace: "app", Files: files("002_b.sql", "001_a.sql", "README.md")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got["app"]) != 2 || got["app"][0].version != 1 || got["app"][1].name != "b" {
		t.Fatalf("load: %+v", got["app"])
	}
}

func TestPlan(t *testing.T) {
	loaded, _ := load([]Set{{Namespace: "app", Files: files("001_a.sql", "002_b.sql", "003_c.sql")}})
	ms := loaded["app"]

	pending, newer, err := plan("app", ms, map[int]string{1: ms[0].checksum})
	if err != nil || len(pending) != 2 || len(newer) != 0 {
		t.Fatalf("pending %v newer %v err %v", pending, newer, err)
	}
	_, newer, err = plan("app", ms, map[int]string{1: ms[0].checksum, 2: ms[1].checksum, 3: ms[2].checksum, 4: "x", 7: "y"})
	if err != nil || len(newer) != 2 || newer[0] != 4 {
		t.Fatalf("newer %v err %v", newer, err)
	}

	cases := map[string]map[int]string{
		"changed checksum":     {1: "different"},
		"unknown older":        {1: ms[0].checksum, 2: "x"},
		"gap before applied":   {1: ms[0].checksum, 3: ms[2].checksum},
		"unknown below newest": {1: ms[0].checksum, 2: ms[1].checksum, 3: ms[2].checksum, 0: "x"},
	}
	for name, done := range cases {
		if _, _, err := plan("app", ms, done); err == nil {
			t.Errorf("%s: plan succeeded", name)
		}
	}
}

func TestSSLModeDetection(t *testing.T) {
	cases := []struct {
		dsn  string
		has  bool
		mode string
	}{
		{"postgres://u:p@db.example/app", false, ""},
		{"postgres://u:p@db.example/app?sslmode=require", true, "require"},
		{"host=db.example user=u sslmode=disable", true, "disable"},
		{"host=db.example user=u", false, ""},
	}
	for _, c := range cases {
		if hasSSLMode(c.dsn) != c.has {
			t.Errorf("hasSSLMode(%q) = %v", c.dsn, !c.has)
		}
		if c.has && sslMode(c.dsn) != c.mode {
			t.Errorf("sslMode(%q) = %q", c.dsn, sslMode(c.dsn))
		}
	}
	if got := withSSLMode("postgres://u:p@db.example/app?application_name=x", "verify-full"); !strings.Contains(got, "sslmode=verify-full") || !strings.Contains(got, "application_name=x") {
		t.Errorf("withSSLMode URL: %q", got)
	}
	if got := withSSLMode("host=db.example", "verify-full"); got != "host=db.example sslmode=verify-full" {
		t.Errorf("withSSLMode keyword: %q", got)
	}
}
