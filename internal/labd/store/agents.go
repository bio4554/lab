package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const agentCols = "id, project_id, name, role_prompt, model, credential_id, budget, state, container_id, branch, created_at"

func (s *Store) CreateAgent(ctx context.Context, a NewAgent) (Agent, error) {
	budget := a.Budget
	if len(budget) == 0 {
		budget = []byte("{}")
	}
	rows, _ := s.pool.Query(ctx, `
		INSERT INTO lab.agents (project_id, name, role_prompt, model, credential_id, budget, branch)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+agentCols,
		a.ProjectID, a.Name, a.RolePrompt, a.Model, a.CredentialID, budget, a.Branch)
	agent, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Agent])
	if err != nil {
		return Agent{}, fmt.Errorf("create agent: %w", err)
	}
	return agent, nil
}

func (s *Store) GetAgent(ctx context.Context, id uuid.UUID) (Agent, error) {
	rows, _ := s.pool.Query(ctx,
		"SELECT "+agentCols+" FROM lab.agents WHERE id = $1", id)
	agent, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Agent])
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	if err != nil {
		return Agent{}, fmt.Errorf("get agent: %w", err)
	}
	return agent, nil
}

// ListAgents returns the project's agents ordered by creation.
func (s *Store) ListAgents(ctx context.Context, projectID uuid.UUID) ([]Agent, error) {
	rows, _ := s.pool.Query(ctx,
		"SELECT "+agentCols+" FROM lab.agents WHERE project_id = $1 ORDER BY created_at, id", projectID)
	agents, err := pgx.CollectRows(rows, pgx.RowToStructByName[Agent])
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	return agents, nil
}

func (s *Store) UpdateAgentState(ctx context.Context, id uuid.UUID, state string) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE lab.agents SET state = $2 WHERE id = $1", id, state)
	if err != nil {
		return fmt.Errorf("update agent state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAgentContainer records the agent's container id; nil clears it.
func (s *Store) SetAgentContainer(ctx context.Context, id uuid.UUID, containerID *string) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE lab.agents SET container_id = $2 WHERE id = $1", id, containerID)
	if err != nil {
		return fmt.Errorf("set agent container: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteAgent(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM lab.agents WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
