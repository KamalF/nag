package store

import "context"

// UpsertSubscription records the subscription that the client posted, keyed
// by endpoint. vapidPublic is the key that the *client* says it subscribed
// under, never the server's configured value (§7.1). The conflict branch
// must overwrite p256dh, auth, AND vapid_public, and keep created_at. After a
// key rotation, the browser re-posts the same endpoint under the new key. A
// row whose vapid_public never updates keeps failing forever, and still
// looks healthy (§8.2).
func (s *Store) UpsertSubscription(ctx context.Context, endpoint, p256dh, auth, vapidPublic string, now int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO push_subscriptions (endpoint, p256dh, auth, vapid_public, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(endpoint) DO UPDATE SET
		  p256dh = excluded.p256dh,
		  auth = excluded.auth,
		  vapid_public = excluded.vapid_public`,
		endpoint, p256dh, auth, vapidPublic, now)
	return err
}

// DeleteSubscription removes the row for endpoint. For an unknown endpoint
// it is a no-op, which matches the endpoint's unconditional 204 (§8.2).
func (s *Store) DeleteSubscription(ctx context.Context, endpoint string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM push_subscriptions WHERE endpoint = ?", endpoint)
	return err
}

// SubscriptionCount gives the count for the instance-wide push_subscribed
// of /api/state (§8.2) and for the §10.4 boot line.
func (s *Store) SubscriptionCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM push_subscriptions").Scan(&n)
	return n, err
}

// Subscription is one push_subscriptions row as the sender needs it.
// VAPIDPublic is the key that the subscription is bound to. On a 401/403,
// the sender compares it with the configured key to tell a stale key from a
// bug (§7.1).
type Subscription struct {
	Endpoint    string
	P256dh      string
	Auth        string
	VAPIDPublic string
}

// ListSubscriptions returns every stored subscription, oldest first.
func (s *Store) ListSubscriptions(ctx context.Context) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT endpoint, p256dh, auth, vapid_public FROM push_subscriptions ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var sub Subscription
		if err := rows.Scan(&sub.Endpoint, &sub.P256dh, &sub.Auth, &sub.VAPIDPublic); err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// MismatchedKeyCount counts subscriptions bound to a key other than the
// configured one. The §7.1 boot WARNING uses this count, so the operator
// sees a startup line, not three weeks of silent failures.
func (s *Store) MismatchedKeyCount(ctx context.Context, configuredKey string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM push_subscriptions WHERE vapid_public != ?", configuredKey).Scan(&n)
	return n, err
}
