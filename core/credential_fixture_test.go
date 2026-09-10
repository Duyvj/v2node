package core

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSourceDoesNotReintroduceExposedCredentials(t *testing.T) {
	// Only one-way fingerprints are retained. These credentials were removed
	// from old live-panel fixtures and must never appear in source again.
	blocked := map[string]bool{
		"813d8e2cc60d084f26443e8a707390da719ac868736c7a972663c75bdccc4c16": true,
		"d56efac872a88d7f1a2b43bc4299d95039fc8dc210f9ed2d3c913b1ba1428751": true,
		"f09b1f5fd480795a87f65e357c035707f993b764b1d277f09d99c5f0fe941f3f": true,
		"000daa7d3a910742057b268080c84a4665d470aa6a3fc8d6b4e677d84d770169": true,
		"ef9e6ef19c0a6cece667808046279d70366a3e8aabc30dbb01efeb395e882564": true,
		"e5ed34783e86d9e9a3ee34006ddef888f9c9b1a1cdb2db9fc1087154fef69c28": true,
	}
	root := ".."
	tokens := regexp.MustCompile(`[A-Za-z0-9_/+=-]{20,}`)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "vendor" || entry.Name() == "bin" || entry.Name() == "dist" || entry.Name() == "dist_release") {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".json", ".sh", ".ps1", ".yaml", ".yml":
		default:
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := strings.ReplaceAll(string(data), `\/`, `/`)
		for _, token := range tokens.FindAllString(text, -1) {
			if blocked[fmt.Sprintf("%x", sha256.Sum256([]byte(token)))] {
				t.Errorf("previously exposed credential found in %s (value redacted)", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
