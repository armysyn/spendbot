package store

import "context"

// KVWebPassword is where the page password hash was kept before accounts; on the first start
// with accounts it moves to the first account and is deleted here.
const KVWebPassword = "web_password"

// DeleteKV removes a housekeeping value.
func (s *Store) DeleteKV(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM kv WHERE key = ?", key)
	return err
}
