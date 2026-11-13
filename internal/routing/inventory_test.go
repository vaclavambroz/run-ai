package routing

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveCanonicalAndAlias(t *testing.T) {
	records := []DomainRecord{{
		Domain:        "example.cz",
		Server:        "hosting.example",
		Aliases:       []string{"www.example.cz"},
		DocumentRoots: []string{"/var/www/example"},
		AppType:       "php",
	}}
	canonical, err := Resolve(records, "EXAMPLE.CZ")
	if err != nil || canonical.MatchedAlias || canonical.DocumentRoot != "/var/www/example" {
		t.Fatalf("canonical resolution: %#v, %v", canonical, err)
	}
	alias, err := Resolve(records, "www.example.cz")
	if err != nil || !alias.MatchedAlias || alias.CanonicalDomain != "example.cz" {
		t.Fatalf("alias resolution: %#v, %v", alias, err)
	}
}

func TestResolveFailsClosed(t *testing.T) {
	_, err := Resolve(nil, "missing.example")
	if !errors.Is(err, ErrDomainNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	dup := []DomainRecord{
		{Domain: "one.example", Server: "one", Aliases: []string{"dup.example"}, DocumentRoots: []string{"/one"}},
		{Domain: "two.example", Server: "two", Aliases: []string{"dup.example"}, DocumentRoots: []string{"/two"}},
	}
	_, err = Resolve(dup, "dup.example")
	if !errors.Is(err, ErrDomainAmbiguous) {
		t.Fatalf("expected ambiguous, got %v", err)
	}
	multi := []DomainRecord{{Domain: "multi.example", Server: "one", DocumentRoots: []string{"/one", "/two"}}}
	if _, err := Resolve(multi, "multi.example"); err == nil {
		t.Fatal("multiple roots must fail closed")
	}
}

func TestLoadInventoryRejectsSymlinkAndTrailingData(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "domains.json")
	if err := os.WriteFile(real, []byte(`[{"domain":"example.cz","server":"hosting","document_roots":["/srv/example"]}]`), 0600); err != nil {
		t.Fatal(err)
	}
	records, err := LoadInventory(real, 4096)
	if err != nil || len(records) != 1 {
		t.Fatalf("load: %v, %#v", err, records)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInventory(link, 4096); err == nil {
		t.Fatal("symlink must be rejected")
	}
	if err := os.WriteFile(real, []byte(`[] {}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInventory(real, 4096); err == nil {
		t.Fatal("trailing JSON must be rejected")
	}
}
