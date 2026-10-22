package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
)

func envFloat(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return f, nil
}

var loginName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// dbCmd manages database login roles. It needs a role that may create roles
// (CREATEROLE or superuser), typically the cluster administrator.
func dbCmd(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "create-login" {
		return errors.New("usage: team-memory db create-login --role app|worker|operator --user NAME")
	}
	fs := flag.NewFlagSet("db create-login", flag.ExitOnError)
	role := fs.String("role", "", "app (serve), worker or operator (required)")
	user := fs.String("user", "", "login role name to create (required)")
	_ = fs.Parse(args[1:])
	groups := map[string]string{"app": postgres.RoleApp, "worker": postgres.RoleWorker, "operator": postgres.RoleOperator}
	group, ok := groups[*role]
	if !ok {
		return errors.New("--role must be app, worker or operator")
	}
	if !loginName.MatchString(*user) {
		return errors.New("--user must be a lowercase identifier")
	}
	password := os.Getenv("TEAM_MEMORY_NEW_LOGIN_PASSWORD")
	generated := password == ""
	if generated {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		password = base64.RawURLEncoding.EncodeToString(b)
	}
	return withPool(ctx, func(pool *pgxpool.Pool) error {
		// Identifiers are validated above; the password is passed as a
		// literal through format(%L) on the server, never concatenated.
		var stmt string
		if err := pool.QueryRow(ctx, "SELECT format('CREATE ROLE %I LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD %L IN ROLE %I', $1::text, $2::text, $3::text)",
			*user, password, group).Scan(&stmt); err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("create role: %w", err)
		}
		fmt.Fprintf(os.Stderr, "created login role %s in %s\n", *user, group)
		if generated {
			fmt.Fprintln(os.Stderr, "generated password (shown once; store it in a Kubernetes Secret):")
			fmt.Println(password)
		}
		return nil
	})
}

// principalArg loads a principal by id for commands that act as one.
func principalArg(ctx context.Context, pool *pgxpool.Pool, raw string, authMethod string) (memory.Principal, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return memory.Principal{}, errors.New("--as must be a principal id")
	}
	var out memory.Principal
	err = postgres.WithReadTx(ctx, pool, uuid.Nil, func(tx postgres.Tx) error {
		p, err := tx.Q.GetPrincipal(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("principal %s not found", id)
		}
		if err != nil {
			return err
		}
		if p.DisabledAt != nil {
			return fmt.Errorf("principal %s is disabled", id)
		}
		out = memory.Principal{ID: p.ID, Kind: p.Kind, DisplayName: p.DisplayName, AuthMethod: authMethod}
		return nil
	})
	return out, err
}

// memoryCmd: archive a memory as a named maintainer. The service checks
// that principal's role, as it would for an API call, and attributes the
// lifecycle event to it.
func memoryCmd(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "archive" {
		return errors.New("usage: team-memory memory archive --id ID --as PRINCIPAL --expected-state-version N [--reason TEXT]")
	}
	fs := flag.NewFlagSet("memory archive", flag.ExitOnError)
	id := fs.String("id", "", "memory id (required)")
	as := fs.String("as", "", "maintainer principal id the action is attributed to (required)")
	version := fs.Int64("expected-state-version", 0, "state_version you last read (required)")
	reason := fs.String("reason", "", "why the memory is archived")
	_ = fs.Parse(args[1:])
	mid, err := uuid.Parse(*id)
	if err != nil {
		return errors.New("--id must be a memory id")
	}
	return withPool(ctx, func(pool *pgxpool.Pool) error {
		p, err := principalArg(ctx, pool, *as, "cli")
		if err != nil {
			return err
		}
		m, err := memory.NewService(pool).Archive(ctx, p, memory.ArchiveInput{ID: mid, ExpectedStateVersion: *version, Reason: *reason})
		if err != nil {
			return err
		}
		fmt.Printf("archived %s (state version %d)\n", m.ID, m.StateVersion)
		return nil
	})
}

// idpGroupsCmd manages the optional IdP group to scope role mapping. Grants
// are synchronized at each sign-in through that issuer and marked
// source=idp with the issuer; a sign-in changes only its own issuer's
// grants, and manual grants are never changed by the sync.
func idpGroupsCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: team-memory idp-groups add|remove|list")
	}
	return withPool(ctx, func(pool *pgxpool.Pool) error {
		q := db.New(pool)
		switch args[0] {
		case "add", "remove":
			fs := flag.NewFlagSet("idp-groups "+args[0], flag.ExitOnError)
			issuer := fs.String("issuer", "", "upstream issuer URL, exactly as in its ID tokens (required)")
			group := fs.String("group", "", "group name or ID as it appears in the groups claim (required)")
			scope := fs.String("scope", "", "scope path or id (required)")
			role := fs.String("role", "reader", "reader, writer or maintainer")
			cascade := fs.Bool("cascade", false, "also grant the role on descendant scopes")
			_ = fs.Parse(args[1:])
			if *issuer == "" || *group == "" {
				return errors.New("--issuer and --group are required")
			}
			sc, err := memory.NewService(pool).ResolveScope(ctx, *scope)
			if err != nil {
				return err
			}
			if args[0] == "remove" {
				n, err := q.DeleteIdPGroupMapping(ctx, db.DeleteIdPGroupMappingParams{Issuer: *issuer, GroupName: *group, ScopeID: sc.ID})
				if err != nil {
					return err
				}
				fmt.Printf("removed %d mapping(s); existing grants are removed at each member's next sign-in\n", n)
				return nil
			}
			r, err := memory.ParseRole(*role)
			if err != nil {
				return err
			}
			if err := q.InsertIdPGroupMapping(ctx, db.InsertIdPGroupMappingParams{
				Issuer: *issuer, GroupName: *group, ScopeID: sc.ID, Role: string(r), Cascade: *cascade}); err != nil {
				return err
			}
			fmt.Println("mapped; grants apply at each member's next sign-in")
		case "list":
			rows, err := q.ListIdPGroupMappings(ctx)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ISSUER\tGROUP\tSCOPE\tROLE\tCASCADE")
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\n", r.Issuer, r.GroupName, r.ScopePath, r.Role, r.Cascade)
			}
			return w.Flush()
		default:
			return fmt.Errorf("unknown idp-groups command %q", args[0])
		}
		return nil
	})
}

// auditCmd prints recent audit events of a scope (operator view, including
// denials that carry no scope when --all is set).
func auditCmd(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "list" {
		return errors.New("usage: team-memory audit list [--scope PATH] [--limit N]")
	}
	fs := flag.NewFlagSet("audit list", flag.ExitOnError)
	scope := fs.String("scope", "", "scope path or id (default: all events)")
	limit := fs.Int("limit", 50, "maximum events")
	_ = fs.Parse(args[1:])
	return withPool(ctx, func(pool *pgxpool.Pool) error {
		where, params := "", []any{*limit}
		if *scope != "" {
			sc, err := memory.NewService(pool).ResolveScope(ctx, *scope)
			if err != nil {
				return err
			}
			where, params = "WHERE scope_id = $2", append(params, sc.ID)
		}
		rows, err := pool.Query(ctx, `SELECT occurred_at, coalesce(actor_id::text, '-'), action, target_type, target_id,
			outcome, coalesce(request_id, '-'), details FROM audit_events `+where+` ORDER BY id DESC LIMIT $1`, params...)
		if err != nil {
			return err
		}
		defer rows.Close()
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "TIME\tACTOR\tACTION\tTARGET\tOUTCOME\tREQUEST\tDETAILS")
		for rows.Next() {
			var (
				at                                            time.Time
				actor, action, ttype, tid, outcome, requestID string
				details                                       json.RawMessage
			)
			if err := rows.Scan(&at, &actor, &action, &ttype, &tid, &outcome, &requestID, &details); err != nil {
				return err
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s:%s\t%s\t%s\t%s\n", at.Format(time.RFC3339), actor, action, ttype, tid, outcome, requestID, details)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return w.Flush()
	})
}
