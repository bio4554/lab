package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const agentCols = "id, project_id, name, role_prompt, model, credential_id, budget, state, container_id, branch, status_text, can_spawn, retire_context_tokens, created_at"

func (s *Store) CreateAgent(ctx context.Context, a NewAgent) (Agent, error) {
	budget := a.Budget
	if len(budget) == 0 {
		budget = []byte("{}")
	}
	rows, _ := s.pool.Query(ctx, `
		INSERT INTO lab.agents (project_id, name, role_prompt, model, credential_id, budget, branch, can_spawn, retire_context_tokens)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+agentCols,
		a.ProjectID, a.Name, a.RolePrompt, a.Model, a.CredentialID, budget, a.Branch, a.CanSpawn, a.RetireContextTokens)
	agent, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Agent])
	if isUniqueViolation(err) {
		return Agent{}, fmt.Errorf("agent %q already exists in this project: %w", a.Name, ErrDuplicateName)
	}
	if err != nil {
		return Agent{}, fmt.Errorf("create agent: %w", err)
	}
	return agent, nil
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
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

// ActiveAgents returns every agent whose state is neither stopped nor
// retired — agents that were running when a previous daemon died and
// should be restarted on daemon startup.
func (s *Store) ActiveAgents(ctx context.Context) ([]Agent, error) {
	rows, _ := s.pool.Query(ctx,
		"SELECT "+agentCols+" FROM lab.agents WHERE state NOT IN ($1, $2) ORDER BY created_at, id",
		AgentStateStopped, AgentStateRetired)
	agents, err := pgx.CollectRows(rows, pgx.RowToStructByName[Agent])
	if err != nil {
		return nil, fmt.Errorf("active agents: %w", err)
	}
	return agents, nil
}

// AllAgents returns every agent across all projects, ordered by
// creation. The boot reconciliation sweep walks this against the
// container runtime's listing.
func (s *Store) AllAgents(ctx context.Context) ([]Agent, error) {
	rows, _ := s.pool.Query(ctx,
		"SELECT "+agentCols+" FROM lab.agents ORDER BY created_at, id")
	agents, err := pgx.CollectRows(rows, pgx.RowToStructByName[Agent])
	if err != nil {
		return nil, fmt.Errorf("all agents: %w", err)
	}
	return agents, nil
}

// SetAgentStatusText records the agent's self-reported status string;
// nil clears it.
func (s *Store) SetAgentStatusText(ctx context.Context, id uuid.UUID, status *string) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE lab.agents SET status_text = $2 WHERE id = $1", id, status)
	if err != nil {
		return fmt.Errorf("set agent status text: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAgentBudget replaces the agent's budget limits (see budget.Limits
// for the jsonb contract).
func (s *Store) SetAgentBudget(ctx context.Context, id uuid.UUID, budget json.RawMessage) error {
	if len(budget) == 0 {
		budget = []byte("{}")
	}
	tag, err := s.pool.Exec(ctx,
		"UPDATE lab.agents SET budget = $2 WHERE id = $1", id, budget)
	if err != nil {
		return fmt.Errorf("set agent budget: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAgentCredential rebinds the agent to a credential; nil unbinds.
func (s *Store) SetAgentCredential(ctx context.Context, id uuid.UUID, credentialID *uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE lab.agents SET credential_id = $2 WHERE id = $1", id, credentialID)
	if err != nil {
		return fmt.Errorf("set agent credential: %w", err)
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
