package service

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultProjectIDProductionReferencesStayInDefaults(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	serverRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	allowed := filepath.Clean(filepath.Join(serverRoot, "internal", "service", "defaults.go"))
	defaultProjectIDLiteral := DefaultProjectID
	var violations []string
	if err := filepath.WalkDir(serverRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		//nolint:gosec // Test scans repository source files discovered by WalkDir, not user-controlled paths.
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		content := string(data)
		if !strings.Contains(content, "DefaultProjectID") && !strings.Contains(content, defaultProjectIDLiteral) {
			return nil
		}
		cleanPath := filepath.Clean(path)
		if cleanPath == allowed {
			return nil
		}
		rel, err := filepath.Rel(serverRoot, cleanPath)
		if err != nil {
			rel = cleanPath
		}
		violations = append(violations, rel)
		return nil
	}); err != nil {
		t.Fatalf("scan production references: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("DefaultProjectID must stay in default initialization; project auth middleware resolves /projects/default before services run. Production references: %s", strings.Join(violations, ", "))
	}
}
