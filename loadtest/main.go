// Command loadtest seeds a synthetic Team Memory dataset and measures the
// latency of the MCP search and write paths, portal pages and scope-tree
// navigation (docs/operations/load-test-report.md).
//
//	go run ./loadtest seed  --users users.json         (DATABASE_URL: operator role)
//	go run ./loadtest embed-synthetic --ollama http://127.0.0.1:11434
//	go run ./loadtest run   --base https://memory.example.com --users users.json
//
// The dataset is synthetic engineering data, not pilot evidence.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: loadtest seed|run [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "seed":
		err = seedCmd(ctx, os.Args[2:])
	case "run":
		err = runCmd(ctx, os.Args[2:])
	case "embed-synthetic":
		err = embedSyntheticCmd(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// User is one load-test identity: a token and what it may do.
type User struct {
	ID       string `json:"id"`
	Token    string `json:"token"`
	Scope    string `json:"scope"`      // deepest scope it reads (and writes when Writer)
	Depth    int    `json:"depth"`      // depth of Scope (0 = organization)
	Writer   bool   `json:"writer"`     // may create memories in Scope
	Maintain bool   `json:"maintainer"` // may open the review queue of Scope
}

// Dataset is written by seed and read by run.
type Dataset struct {
	Users     []User   `json:"users"`
	MemoryIDs []string `json:"memory_ids"` // a sample, for memory pages
	Scopes    int      `json:"scopes"`
	Memories  int      `json:"memories"`
	Principal int      `json:"principals"`
	Grants    int      `json:"grants"`
	MaxDepth  int      `json:"max_depth"`
}

func readDataset(path string) (Dataset, error) {
	var d Dataset
	b, err := os.ReadFile(path)
	if err != nil {
		return d, err
	}
	err = json.Unmarshal(b, &d)
	if err == nil && len(d.Users) == 0 {
		err = errors.New("dataset has no users")
	}
	return d, err
}

func flags(name string, args []string, def func(*flag.FlagSet)) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	def(fs)
	_ = fs.Parse(args)
	return fs
}
