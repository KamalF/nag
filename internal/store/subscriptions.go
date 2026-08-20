package store

import "context"

// UpsertSubscription records the subscription the client posted, keyed by
// endpoint. vapidPublic is the key the *client* says it subscribed under —
// never the server's configured value (§7.1). The conflict branch must
// overwrite p256dh, auth, AND vapid_public while preserving created_at:
// after a key rotation the browser re-posts the same endpoint under the
// new key, and a row whose vapid_public never updates keeps failing
// forever while looking healthy (§8.2).
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

// DeleteSubscription removes the row for endpoint; deleting a stranger is
// a no-op, matching the endpoint's unconditional 204 (§8.2).
func (s *Store) DeleteSubscription(ctx context.Context, endpoint string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM push_subscriptions WHERE endpoint = ?", endpoint)
	return err
}

// SubscriptionCount backs /api/state's instance-wide push_subscribed
// (§8.2) and the §10.4 boot line.
func (s *Store) SubscriptionCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM push_subscriptions").Scan(&n)
	return n, err
}
