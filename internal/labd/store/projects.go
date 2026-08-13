package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const projectCols = "id, name, origin_kind, origin, stack, default_credential_id, created_at"

func (s *Store) CreateProject(ctx context.Context, p NewProject) (Project, error) {
	rows, _ := s.pool.Query(ctx, `
		INSERT INTO lab.projects (name, origin_kind, origin, stack, default_credential_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+projectCols,
		p.Name, p.OriginKind, p.Origin, p.Stack, p.DefaultCredentialID)
	proj, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Project])
	if isUniqueViolation(err) {
		return Project{}, fmt.Errorf("project %q already exists: %w", p.Name, ErrDuplicateName)
	}
	if err != nil {
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	return proj, nil
}

func (s *Store) GetProject(ctx context.Context, id uuid.UUID) (Project, error) {
	rows, _ := s.pool.Query(ctx,
		"SELECT "+projectCols+" FROM lab.projects WHERE id = $1", id)
	proj, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Project])
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("get project: %w", err)
	}
	return proj, nil
}

func (s *Store) GetProjectByName(ctx context.Context, name string) (Project, error) {
	rows, _ := s.pool.Query(ctx,
		"SELECT "+projectCols+" FROM lab.projects WHERE name = $1", name)
	proj, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Project])
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("get project by name: %w", err)
	}
	return proj, nil
}

func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, _ := s.pool.Query(ctx,
		"SELECT "+projectCols+" FROM lab.projects ORDER BY created_at, id")
	projects, err := pgx.CollectRows(rows, pgx.RowToStructByName[Project])
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return projects, nil
}

func (s *Store) DeleteProject(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM lab.projects WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
