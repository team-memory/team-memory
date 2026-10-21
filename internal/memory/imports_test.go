package memory_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The memory engine must stay independent of transport adapters.
func TestNoAdapterImports(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/team-memory/team-memory/internal/memory").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.HasPrefix(dep, "github.com/team-memory/team-memory/internal/mcp") ||
			strings.HasPrefix(dep, "github.com/modelcontextprotocol/") {
			t.Errorf("internal/memory depends on %s", dep)
		}
	}
}
