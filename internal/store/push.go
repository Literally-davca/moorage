package store

import (
	"context"
	"time"
)

// SettingVAPIDKey holds the hub's Web Push signing key (PKCS#8, base64).
const SettingVAPIDKey = "vapid_private_key"

// PushSubscription is a browser that accepted Web Push notifications.
type PushSubscription struct {
	ID       int64
	UserID   int64
	Endpoint string
	P256DH   string // base64url, the browser's ECDH public key
	Auth     string // base64url, the browser's auth secret
	Created  time.Time
}

// SavePushSubscription stores a subscription, moving an existing endpoint to userID.
func (s *Store) SavePushSubscription(ctx context.Context, p PushSubscription) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth, created) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(endpoint) DO UPDATE SET user_id = excluded.user_id, p256dh = excluded.p256dh, auth = excluded.auth`,
		p.UserID, p.Endpoint, p.P256DH, p.Auth, time.Now().Unix())
	return err
}

// DeletePushSubscription removes a subscription by endpoint (userID 0 = any user).
func (s *Store) DeletePushSubscription(ctx context.Context, userID int64, endpoint string) error {
	if userID == 0 {
		_, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint)
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE endpoint = ? AND user_id = ?`, endpoint, userID)
	return err
}

// PushSubscriptions lists subscriptions (userID 0 = all users).
func (s *Store) PushSubscriptions(ctx context.Context, userID int64) ([]PushSubscription, error) {
	q := `SELECT id, user_id, endpoint, p256dh, auth, created FROM push_subscriptions`
	var args []any
	if userID != 0 {
		q += ` WHERE user_id = ?`
		args = append(args, userID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushSubscription
	for rows.Next() {
		var p PushSubscription
		var created int64
		if err := rows.Scan(&p.ID, &p.UserID, &p.Endpoint, &p.P256DH, &p.Auth, &created); err != nil {
			return nil, err
		}
		p.Created = time.Unix(created, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}
