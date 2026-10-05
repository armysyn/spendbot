package analysis

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"spendbot/internal/clickhouse"
	"spendbot/internal/store"
)

var almaty, _ = time.LoadLocation("Asia/Almaty")

// testCH connects to ClickHouse at CLICKHOUSE_TEST_URL (http://localhost:8123, say) and
// creates a separate database per test. Without the variable the test is skipped.
func testCH(t *testing.T) *clickhouse.Client {
	t.Helper()
	url := os.Getenv("CLICKHOUSE_TEST_URL")
	if url == "" {
		t.Skip("CLICKHOUSE_TEST_URL is not set")
	}
	ch := clickhouse.New(url, fmt.Sprintf("spend_test_%d", time.Now().UnixNano()))
	ctx := context.Background()
	if err := Migrate(ctx, ch, "Asia/Almaty"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ch.ExecRaw(context.Background(), "DROP DATABASE IF EXISTS "+ch.DB()) })
	return ch
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
