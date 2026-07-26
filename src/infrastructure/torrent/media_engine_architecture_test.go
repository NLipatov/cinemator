package torrent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestExternalMediaProcessesAreIsolatedBehindMediaEngine(t *testing.T) {
	processAPIs := map[string]struct{}{
		"SampleAnalyzer":          {},
		"GenerateVideoWindow":     {},
		"GenerateDirectWindow":    {},
		"GenerateSubtitleSegment": {},
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	files := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() ||
			filepath.Ext(name) != ".go" ||
			strings.HasSuffix(name, "_test.go") ||
			name == "media_engine.go" {
			continue
		}
		file, err := parser.ParseFile(files, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ffmpegPackages := make(map[string]struct{})
		for _, imported := range file.Imports {
			importPath, err := strconv.Unquote(imported.Path.Value)
			if err != nil || importPath != "cinemator/infrastructure/ffmpeg" {
				continue
			}
			localName := "ffmpeg"
			if imported.Name != nil {
				localName = imported.Name.Name
			}
			if localName == "." {
				t.Errorf("%s uses a dot import for the FFmpeg package", files.Position(imported.Pos()))
				continue
			}
			ffmpegPackages[localName] = struct{}{}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			if _, imported := ffmpegPackages[pkg.Name]; !imported {
				return true
			}
			if _, external := processAPIs[selector.Sel.Name]; !external {
				return true
			}
			t.Errorf(
				"%s calls external media API ffmpeg.%s outside media_engine.go",
				files.Position(selector.Pos()),
				selector.Sel.Name,
			)
			return true
		})
	}
}
