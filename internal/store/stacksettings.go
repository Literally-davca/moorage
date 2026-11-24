package store

import (
	"context"
	"database/sql"
	"errors"
)

// Deploy strategies of a stack (engine.StrategyStandard / StrategyRolling).
const (
	StrategyStandard = "standard"
	StrategyRolling  = "rolling"
)

// StackStrategy returns the deploy strategy of a stack (standard when never set).
func (s *Store) StackStrategy(ctx context.Context, hostID, stack string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT strategy FROM stack_settings WHERE host_id = ? AND stack = ?`, hostID, stack).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) || v == "" {
		return StrategyStandard, nil
	}
	return v, err
}

// SetStackStrategy stores the deploy strategy of a stack.
func (s *Store) SetStackStrategy(ctx context.Context, hostID, stack, strategy string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO stack_settings (host_id, stack, strategy) VALUES (?, ?, ?)
		ON CONFLICT(host_id, stack) DO UPDATE SET strategy = excluded.strategy`, hostID, stack, strategy)
	return err
}
