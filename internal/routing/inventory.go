package routing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var (
	ErrDomainNotFound  = errors.New("domain not found in inventory")
	ErrDomainAmbiguous = errors.New("domain resolves to multiple inventory records")
)

// DomainRecord is the small, non-secret subset of hosting-inventory/domains.json
// needed to select the execution host and application root.
type DomainRecord struct {
	Domain               string   `json:"domain"`
	Server               string   `json:"server"`
	Aliases              []string `json:"aliases"`
	DocumentRoots        []string `json:"document_roots"`
	VirtualDocumentRoots []string `json:"virtual_document_roots"`
	AppType              string   `json:"app_type"`
}

type Resolution struct {
	RequestedDomain string `json:"requested_domain"`
	CanonicalDomain string `json:"canonical_domain"`
	Server          string `json:"server"`
	DocumentRoot    string `json:"document_root"`
	AppType         string `json:"app_type,omitempty"`
	MatchedAlias    bool   `json:"matched_alias"`
}

// LoadInventory reads a bounded regular file. Symlinks are rejected so a
// privileged launcher cannot be redirected to attacker-controlled inventory.
func LoadInventory(path string, maxBytes int64) ([]DomainRecord, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("inventory path must be absolute")
	}
	if maxBytes <= 0 {
		return nil, errors.New("inventory size limit must be positive")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inventory stat: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("inventory must be a regular non-symlink file")
	}
	if info.Size() > maxBytes {
		return nil, errors.New("inventory exceeds size limit")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("inventory open: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("inventory read: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("inventory exceeds size limit")
	}
	var records []DomainRecord
	dec := json.NewDecoder(strings.NewReader(string(data)))
	if err := dec.Decode(&records); err != nil {
		return nil, fmt.Errorf("inventory JSON: %w", err)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("inventory contains trailing JSON")
	}
	return records, nil
}

func Resolve(records []DomainRecord, domain string) (Resolution, error) {
	domain = strings.TrimSpace(strings.ToLower(domain))
	if domain == "" {
		return Resolution{}, ErrDomainNotFound
	}
	var matches []Resolution
	for _, record := range records {
		canonical := strings.ToLower(strings.TrimSpace(record.Domain))
		if canonical == "" || strings.TrimSpace(record.Server) == "" {
			continue
		}
		matchedAlias := false
		matched := canonical == domain
		if !matched {
			for _, alias := range record.Aliases {
				if strings.ToLower(strings.TrimSpace(alias)) == domain {
					matched = true
					matchedAlias = true
					break
				}
			}
		}
		if !matched {
			continue
		}
		root, err := uniqueRoot(record)
		if err != nil {
			return Resolution{}, fmt.Errorf("%s: %w", canonical, err)
		}
		matches = append(matches, Resolution{
			RequestedDomain: domain,
			CanonicalDomain: canonical,
			Server:          strings.TrimSpace(record.Server),
			DocumentRoot:    root,
			AppType:         strings.TrimSpace(record.AppType),
			MatchedAlias:    matchedAlias,
		})
	}
	if len(matches) == 0 {
		return Resolution{}, ErrDomainNotFound
	}
	if len(matches) != 1 {
		return Resolution{}, ErrDomainAmbiguous
	}
	return matches[0], nil
}

func uniqueRoot(record DomainRecord) (string, error) {
	roots := append([]string{}, record.DocumentRoots...)
	roots = append(roots, record.VirtualDocumentRoots...)
	seen := make(map[string]struct{})
	for _, root := range roots {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "." || !filepath.IsAbs(root) {
			continue
		}
		seen[root] = struct{}{}
	}
	if len(seen) == 0 {
		return "", errors.New("no absolute document root")
	}
	values := make([]string, 0, len(seen))
	for root := range seen {
		values = append(values, root)
	}
	sort.Strings(values)
	if len(values) != 1 {
		return "", errors.New("multiple document roots require an explicit hosting profile rule")
	}
	return values[0], nil
}
