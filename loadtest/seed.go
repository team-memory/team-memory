package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/auth"
	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
)

// vocabulary gives memories searchable, overlapping terms.
var vocabulary = strings.Fields(`postgres kafka redis kubernetes helm terraform grpc graphql oauth oidc saml
ledger payments billing invoices refunds retries idempotency migrations schema index vacuum replication
backup restore latency throughput cache eviction ttl queue worker cron deploy rollback canary feature-flag
tracing metrics alerts logging audit encryption tls certificates secrets rotation rbac tenancy sharding
pagination cursor timeout circuit-breaker backpressure batching streaming webhook ingestion embeddings
search ranking pgvector hnsw fulltext tokenizer locale timezone currency rounding decimal precision`)

var kinds = []string{"fact", "decision", "convention", "lesson", "context"}

func words(r *rand.Rand, n int) string {
	w := make([]string, n)
	for i := range w {
		w[i] = vocabulary[r.IntN(len(vocabulary))]
	}
	return strings.Join(w, " ")
}

type scopeNode struct {
	path  string
	depth int
}

func seedCmd(ctx context.Context, args []string) error {
	var out, org string
	var branching, perLeaf, perInner, principals, users, workers int
	flags("seed", args, func(fs *flag.FlagSet) {
		fs.StringVar(&out, "users", "users.json", "where to write the dataset (tokens included: keep it private)")
		fs.StringVar(&org, "org", "load", "organization slug")
		fs.IntVar(&branching, "branching", 4, "children per scope")
		fs.IntVar(&perLeaf, "memories-per-project", 60, "memories in each leaf scope")
		fs.IntVar(&perInner, "memories-per-inner", 10, "memories in each non-leaf scope")
		fs.IntVar(&principals, "principals", 1000, "principals with grants")
		fs.IntVar(&users, "load-users", 64, "principals that get tokens for the run")
		fs.IntVar(&workers, "workers", 8, "parallel writers")
	})
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL (operator role) is not set")
	}
	pool, err := postgres.Open(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := postgres.Migrate(ctx, pool); err != nil {
		return err
	}
	svc := memory.NewService(pool)
	q := db.New(pool)
	r := rand.New(rand.NewPCG(42, 1))
	start := time.Now()

	owner, err := q.CreatePrincipal(ctx, db.CreatePrincipalParams{Kind: "human", DisplayName: "Load owner"})
	if err != nil {
		return err
	}
	if _, err := svc.CreateOrganization(ctx, owner.ID, org, "Load test"); err != nil {
		return err
	}
	levels := []string{"organization", "team", "group", "subgroup", "project"}
	if _, err := svc.ConfigureOrg(ctx, org, levels, len(levels)); err != nil {
		return err
	}
	if err := svc.AddMember(ctx, uuid.Nil, org, owner.ID, memory.RoleMaintainer, true); err != nil {
		return err
	}
	// The tree: branching^d scopes at depth d, down to projects.
	all := []scopeNode{{path: org}}
	frontier := all
	for d := 1; d < len(levels); d++ {
		var next []scopeNode
		for _, p := range frontier {
			for i := range branching {
				sc, err := svc.CreateScope(ctx, uuid.Nil, p.path, levels[d], fmt.Sprintf("%s%d", levels[d][:1], i), "")
				if err != nil {
					return err
				}
				next = append(next, scopeNode{path: sc.Path, depth: d})
			}
		}
		all = append(all, next...)
		frontier = next
	}
	fmt.Printf("scopes: %d (depth %d) in %s\n", len(all), len(levels)-1, time.Since(start).Round(time.Millisecond))

	// Memories, written through the service so revisions, events, jobs and
	// (when enabled) mirror state are exactly what production writes.
	ownerP := memory.Principal{ID: owner.ID, Kind: "human", DisplayName: "Load owner", AuthMethod: memory.AuthPAT}
	type task struct {
		scope string
		seed  uint64
	}
	tasks := make(chan task)
	var created atomic.Int64
	var mu sync.Mutex
	var sample []string
	var firstErr error
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range tasks {
				rr := rand.New(rand.NewPCG(t.seed, 7))
				m, err := svc.Create(ctx, ownerP, memory.CreateInput{Scope: t.scope, Kind: kinds[rr.IntN(len(kinds))],
					Title:     strings.ToUpper(words(rr, 1)[:1]) + words(rr, 5)[1:],
					Content:   "We " + words(rr, 40) + ".",
					Rationale: "Because " + words(rr, 15) + "."})
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				if err == nil && len(sample) < 2000 && rr.IntN(8) == 0 {
					sample = append(sample, m.ID.String())
				}
				mu.Unlock()
				if n := created.Add(1); n%2000 == 0 {
					fmt.Printf("  %d memories\n", n)
				}
			}
		}()
	}
	n := uint64(0)
	for _, s := range all {
		k := perInner
		if s.depth == len(levels)-1 {
			k = perLeaf
		}
		for range k {
			n++
			select {
			case tasks <- task{s.path, n}:
			case <-ctx.Done():
			}
		}
	}
	close(tasks)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	fmt.Printf("memories: %d in %s\n", created.Load(), time.Since(start).Round(time.Second))

	// Principals with grants at every depth: cascading readers high in the
	// tree, writers and maintainers on projects.
	pats := auth.NewPATs(pool)
	ds := Dataset{Scopes: len(all), Memories: int(created.Load()), MaxDepth: len(levels) - 1, MemoryIDs: sample}
	grants := 0
	for i := range principals {
		p, err := q.CreatePrincipal(ctx, db.CreatePrincipalParams{Kind: "human", DisplayName: fmt.Sprintf("Load user %04d", i)})
		if err != nil {
			return err
		}
		home := all[r.IntN(len(all))]
		role, cascade := memory.RoleReader, home.depth < len(levels)-1
		switch {
		case home.depth == len(levels)-1 && i%3 == 0:
			role = memory.RoleMaintainer
		case home.depth == len(levels)-1:
			role = memory.RoleWriter
		}
		if err := svc.AddMember(ctx, uuid.Nil, home.path, p.ID, role, cascade); err != nil {
			return err
		}
		grants++
		// Extra non-cascading grants elsewhere make access rows realistic.
		for range 3 {
			other := all[r.IntN(len(all))]
			if err := svc.AddMember(ctx, uuid.Nil, other.path, p.ID, memory.RoleReader, false); err == nil {
				grants++
			}
		}
		if i < users {
			tok, err := pats.Issue(ctx, p.ID, "loadtest", 7*24*time.Hour)
			if err != nil {
				return err
			}
			ds.Users = append(ds.Users, User{ID: p.ID.String(), Token: tok.Secret, Scope: home.path, Depth: home.depth,
				Writer: role != memory.RoleReader, Maintain: role == memory.RoleMaintainer})
		}
	}
	ds.Principal, ds.Grants = principals+1, grants
	if err := analyze(ctx, pool); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(ds, "", "  ")
	if err := os.WriteFile(out, b, 0o600); err != nil {
		return err
	}
	fmt.Printf("principals: %d, grants: %d, load users: %d; total %s; wrote %s\n",
		ds.Principal, grants, len(ds.Users), time.Since(start).Round(time.Second), out)
	return nil
}

func analyze(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, "ANALYZE")
	return err
}
