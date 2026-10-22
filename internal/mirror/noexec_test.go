package mirror

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/file"
)

// TestNoProcessExecution enforces that the service never runs another
// program (in particular a git binary): no non-test package of this module
// imports os/exec or execabs.
func TestNoProcessExecution(t *testing.T) {
	root := filepath.Join("..", "..")
	forbidden := map[string]bool{"os/exec": true, "golang.org/x/sys/execabs": true}
	checked := 0
	for _, dir := range []string{"cmd", "internal", "migrations"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			checked++
			for _, imp := range f.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				if forbidden[p] {
					t.Errorf("%s imports %s; the service must not execute programs", path, p)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked < 50 {
		t.Fatalf("only %d files checked; wrong root?", checked)
	}
}

// TestFileTransportIsInProcess guards the one place go-git would execute
// git-upload-pack/git-receive-pack: the file:// transport.
func TestFileTransportIsInProcess(t *testing.T) {
	if client.Protocols["file"] == file.DefaultClient {
		t.Fatal("file:// still uses go-git's exec-based client")
	}
}
