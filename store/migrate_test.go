package store

import (
	"strings"
	"testing"
)

func TestLoadFindsTheEmbeddedSchema(t *testing.T) {
	all, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("Load returned no migrations")
	}

	first := all[0]
	if first.Version != "0001" || first.Name != "vibe_agent" {
		t.Fatalf("first migration = %s_%s, want 0001_vibe_agent", first.Version, first.Name)
	}
	if len(first.Checksum) != 64 {
		t.Fatalf("checksum length = %d, want 64 hex characters", len(first.Checksum))
	}
	for _, table := range []string{
		"CREATE TABLE vibe_profiles",
		"CREATE TABLE vibe_sessions",
		"CREATE TABLE vibe_runs",
		"CREATE TABLE vibe_turns",
	} {
		if !strings.Contains(first.SQL, table) {
			t.Errorf("schema does not contain %q", table)
		}
	}
}

func TestParseName(t *testing.T) {
	version, name, ok := parseName("0012_add_widgets.sql")
	if !ok || version != "0012" || name != "add_widgets" {
		t.Fatalf("parseName = %q, %q, %t", version, name, ok)
	}

	for _, filename := range []string{
		"12_short.sql",
		"0012.sql",
		"0012_.sql",
		"abcd_name.sql",
		"0012_add_widgets.txt",
	} {
		if _, _, ok := parseName(filename); ok {
			t.Errorf("parseName(%q) accepted a bad name", filename)
		}
	}
}

func TestChecksumIgnoresLineEndings(t *testing.T) {
	unix := checksum([]byte("CREATE TABLE a ();\nCREATE TABLE b ();\n"))
	windows := checksum([]byte("CREATE TABLE a ();\r\nCREATE TABLE b ();\r\n"))
	if unix != windows {
		t.Fatal("checksums differ between LF and CRLF checkouts")
	}
}
