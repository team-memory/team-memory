package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
)

// ErrAlreadyInitialized is returned when init runs against a database that
// already has an organization. Init never replaces an existing owner.
var ErrAlreadyInitialized = errors.New("already initialized: an organization exists; the existing owner was not changed")

// InitOptions configures first-run bootstrap.
type InitOptions struct {
	OrgSlug     string
	OrgName     string
	ProjectSlug string
	OwnerName   string
	TokenTTL    time.Duration
}

// InitResult is printed once by `team-memory init`.
type InitResult struct {
	OrgID       uuid.UUID
	OrgPath     string
	ProjectPath string
	OwnerID     uuid.UUID
	Token       IssuedToken
}

// Init creates the first organization, a default project, the owner principal
// (maintainer with a cascading grant on the organization) and the owner's
// first personal access token, atomically.
func Init(ctx context.Context, pool *pgxpool.Pool, opts InitOptions) (InitResult, error) {
	var res InitResult
	err := postgres.WithTx(ctx, pool, uuid.Nil, func(tx postgres.Tx) error {
		// Serialize concurrent init runs.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('team-memory-init'))"); err != nil {
			return err
		}
		n, err := tx.Q.CountOrganizations(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return ErrAlreadyInitialized
		}
		owner, err := tx.Q.CreatePrincipal(ctx, db.CreatePrincipalParams{Kind: "human", DisplayName: opts.OwnerName})
		if err != nil {
			return err
		}
		root, err := tx.Q.CreateRootScope(ctx, db.CreateRootScopeParams{
			ID: uuid.New(), Level: "organization", Slug: opts.OrgSlug, Name: opts.OrgName, CreatedBy: &owner.ID,
		})
		if err != nil {
			return err
		}
		if _, err := tx.Q.InsertOrgSettings(ctx, db.InsertOrgSettingsParams{
			OrgID: root.ID, AllowedLevels: []string{"organization", "team", "project"}, MaxDepth: 5, OwnerPrincipalID: owner.ID,
		}); err != nil {
			return err
		}
		project, err := tx.Q.CreateChildScope(ctx, db.CreateChildScopeParams{
			Level: "project", Slug: opts.ProjectSlug, Name: opts.ProjectSlug, CreatedBy: &owner.ID, ParentID: root.ID,
		})
		if err != nil {
			return err
		}
		if _, err := tx.Q.UpsertScopeMember(ctx, db.UpsertScopeMemberParams{
			PrincipalID: owner.ID, Role: "maintainer", Cascade: true, GrantedBy: &owner.ID, ScopeID: root.ID,
		}); err != nil {
			return err
		}
		tok, err := IssueInTx(ctx, tx.Q, owner.ID, "init", opts.TokenTTL, time.Now())
		if err != nil {
			return err
		}
		if err := tx.Q.InsertAuditEvent(ctx, db.InsertAuditEventParams{
			OrgID: &root.ID, ActorID: &owner.ID, Action: "org.init", TargetType: "organization",
			TargetID: root.ID.String(), ScopeID: &root.ID, Outcome: "success", Details: []byte(`{}`),
		}); err != nil {
			return err
		}
		res = InitResult{OrgID: root.ID, OrgPath: root.Path, ProjectPath: project.Path, OwnerID: owner.ID, Token: tok}
		return nil
	})
	return res, err
}
