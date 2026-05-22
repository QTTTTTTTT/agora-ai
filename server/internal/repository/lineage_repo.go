package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fundai/server/internal/lineage"
)

type AgentLineageParent struct {
	AgentID         string
	AgentName       string
	AgentRole       string
	AgentFocus      sql.NullString
	OwnerUserID     string
	DerivedVia      string
	SourceListingID sql.NullString
	CreatedAt       time.Time
}

type LineageRepo struct {
	db *sql.DB
}

func NewLineageRepo(db *sql.DB) *LineageRepo {
	return &LineageRepo{db: db}
}

func (r *LineageRepo) Ancestors(agentID string) (map[string]struct{}, error) {
	rows, err := r.db.QueryContext(context.Background(), `SELECT ancestor_agent_id FROM agent_lineage_closure WHERE descendant_agent_id = $1`, agentID)
	if err != nil {
		return nil, fmt.Errorf("lineage_repo: ancestors: %w", err)
	}
	defer rows.Close()
	return scanLineageIDSet(rows)
}

func (r *LineageRepo) Descendants(agentID string) (map[string]struct{}, error) {
	rows, err := r.db.QueryContext(context.Background(), `SELECT descendant_agent_id FROM agent_lineage_closure WHERE ancestor_agent_id = $1`, agentID)
	if err != nil {
		return nil, fmt.Errorf("lineage_repo: descendants: %w", err)
	}
	defer rows.Close()
	return scanLineageIDSet(rows)
}

func scanLineageIDSet(rows *sql.Rows) (map[string]struct{}, error) {
	ids := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("lineage_repo: scan id: %w", err)
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}

func (r *LineageRepo) AddEdge(e lineage.Edge) error {
	tx, err := r.db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("lineage_repo: begin add edge: %w", err)
	}
	defer tx.Rollback()
	if err := r.AddEdgeWithTx(context.Background(), tx, e); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lineage_repo: commit add edge: %w", err)
	}
	return nil
}

func (r *LineageRepo) AddEdgeWithTx(ctx context.Context, tx *sql.Tx, e lineage.Edge) error {
	if tx == nil {
		return ErrNoTx
	}
	if err := e.Validate(); err != nil {
		return err
	}
	var cycleExists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM agent_lineage_closure WHERE ancestor_agent_id = $1 AND descendant_agent_id = $2)`, e.ChildAgentID, e.ParentAgentID).Scan(&cycleExists); err != nil {
		return fmt.Errorf("lineage_repo: cycle check: %w", err)
	}
	if cycleExists {
		return &lineage.ErrCycle{ChildAgentID: e.ChildAgentID, ParentAgentID: e.ParentAgentID, Path: []string{e.ParentAgentID, e.ChildAgentID}}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_lineage (child_agent_id, parent_agent_id, derived_via, source_listing_id, source_subscription_id)
		 VALUES ($1, $2, $3, NULLIF($4, '')::uuid, NULLIF($5, '')::uuid)
		 ON CONFLICT (child_agent_id, parent_agent_id) DO NOTHING`,
		e.ChildAgentID, e.ParentAgentID, string(e.Via), e.SourceListingID, e.SourceSubscriptionID,
	); err != nil {
		return fmt.Errorf("lineage_repo: insert edge: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`WITH new_ancestors AS (
			SELECT $2::uuid AS ancestor_agent_id, 0 AS depth
			UNION ALL
			SELECT ancestor_agent_id, depth FROM agent_lineage_closure WHERE descendant_agent_id = $2
		), new_descendants AS (
			SELECT $1::uuid AS descendant_agent_id, 0 AS depth
			UNION ALL
			SELECT descendant_agent_id, depth FROM agent_lineage_closure WHERE ancestor_agent_id = $1
		)
		INSERT INTO agent_lineage_closure (ancestor_agent_id, descendant_agent_id, depth)
		SELECT a.ancestor_agent_id, d.descendant_agent_id, a.depth + d.depth + 1
		FROM new_ancestors a CROSS JOIN new_descendants d
		WHERE a.ancestor_agent_id <> d.descendant_agent_id
		ON CONFLICT (ancestor_agent_id, descendant_agent_id)
		DO UPDATE SET depth = LEAST(agent_lineage_closure.depth, EXCLUDED.depth)`,
		e.ChildAgentID, e.ParentAgentID,
	); err != nil {
		return fmt.Errorf("lineage_repo: update closure: %w", err)
	}
	return nil
}

func (r *LineageRepo) OwnerOfAgent(agentID string) (string, error) {
	var owner string
	err := r.db.QueryRowContext(context.Background(), `SELECT user_id FROM agents WHERE id = $1`, agentID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lineage_repo: owner of agent: %w", err)
	}
	return owner, nil
}

func (r *LineageRepo) ListParents(ctx context.Context, agentID string) ([]AgentLineageParent, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT a.id, a.name, a.role, a.focus, a.user_id, l.derived_via, l.source_listing_id, l.created_at
		 FROM agent_lineage l
		 JOIN agents a ON a.id = l.parent_agent_id
		 WHERE l.child_agent_id = $1
		 ORDER BY l.created_at DESC`, agentID)
	if err != nil {
		return nil, fmt.Errorf("lineage_repo: list parents: %w", err)
	}
	defer rows.Close()
	var parents []AgentLineageParent
	for rows.Next() {
		var parent AgentLineageParent
		if err := rows.Scan(&parent.AgentID, &parent.AgentName, &parent.AgentRole, &parent.AgentFocus, &parent.OwnerUserID, &parent.DerivedVia, &parent.SourceListingID, &parent.CreatedAt); err != nil {
			return nil, fmt.Errorf("lineage_repo: scan parent: %w", err)
		}
		parents = append(parents, parent)
	}
	return parents, rows.Err()
}
