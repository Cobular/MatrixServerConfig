package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.yml")
	data := []byte(`
version: 1
shared_history: false
users:
  "@user:example.com":
    rooms:
      - "!room:example.com"
  "@steward:example.com":
    shared_history: true
    rooms:
      - "!room:example.com"
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := loadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || !entries[0].SharedHistory || entries[1].SharedHistory {
		t.Fatalf("unexpected entries: %#v", entries)
	}
}

func TestLoadManifestRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.yml")
	if err := os.WriteFile(path, []byte("version: 1\nunknown: true\nusers: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadManifest(path); err == nil {
		t.Fatal("expected unknown field error")
	}
}

func TestPackageBaseNameIsStableAndUnique(t *testing.T) {
	first := packageBaseName("@user:example.com")
	second := packageBaseName("@user:other.example.com")
	if first == second || first != packageBaseName("@user:example.com") {
		t.Fatalf("unexpected package names: %q %q", first, second)
	}
}
