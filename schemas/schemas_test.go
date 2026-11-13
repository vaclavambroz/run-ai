package schemas_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSchemasAreClosedJSONDocuments(t *testing.T) {
	paths, err := filepath.Glob("*.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 4 {
		t.Fatalf("schema count = %d, want 4", len(paths))
	}
	seenIDs := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			payload, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if err := json.Unmarshal(payload, &document); err != nil {
				t.Fatal(err)
			}
			if document["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
				t.Fatal("unexpected JSON schema dialect")
			}
			id, ok := document["$id"].(string)
			if !ok || id == "" {
				t.Fatal("missing schema id")
			}
			if _, duplicate := seenIDs[id]; duplicate {
				t.Fatalf("duplicate schema id %q", id)
			}
			seenIDs[id] = struct{}{}
			if document["additionalProperties"] != false {
				t.Fatal("top-level schema is not closed")
			}
		})
	}
}
