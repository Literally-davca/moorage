package store

import (
	"context"
	"time"
)

// AssistantCommand is a saved assistant prompt a user runs as /name (M19I).
type AssistantCommand struct {
	ID      int64     `json:"id"`
	Name    string    `json:"name"`
	Text    string    `json:"text"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// MaxAssistantCommands per user.
const MaxAssistantCommands = 50

// AssistantCommands lists a user's saved commands by name.
func (s *Store) AssistantCommands(ctx context.Context, userID int64) ([]AssistantCommand, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, text, created, updated FROM assistant_commands WHERE user_id = ? ORDER BY name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssistantCommand{}
	for rows.Next() {
		var c AssistantCommand
		var cr, up int64
		if err := rows.Scan(&c.ID, &c.Name, &c.Text, &cr, &up); err != nil {
			return nil, err
		}
		c.Created, c.Updated = time.Unix(cr, 0), time.Unix(up, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveAssistantCommand creates or replaces the command name of a user. It refuses a
// new one past MaxAssistantCommands (ok=false).
func (s *Store) SaveAssistantCommand(ctx context.Context, userID int64, name, text string) (bool, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx, `UPDATE assistant_commands SET text = ?, updated = ? WHERE user_id = ? AND name = ?`, text, now, userID, name)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM assistant_commands WHERE user_id = ?`, userID).Scan(&count); err != nil {
		return false, err
	}
	if count >= MaxAssistantCommands {
		return false, nil
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO assistant_commands (user_id, name, text, created, updated) VALUES (?, ?, ?, ?, ?)`, userID, name, text, now, now)
	return err == nil, err
}

// DeleteAssistantCommand removes a user's command.
func (s *Store) DeleteAssistantCommand(ctx context.Context, userID int64, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM assistant_commands WHERE user_id = ? AND name = ?`, userID, name)
	return err
}
