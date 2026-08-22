package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestScheduledTestObserveOnlySuppressesAccountStateWrites(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	repo := newAccountRepositoryWithSQL(nil, db, nil)
	ctx := service.WithScheduledTestObserveOnly(context.Background())

	require.NoError(t, repo.SetError(ctx, 7, "auth failed"))
	require.NoError(t, repo.SetRateLimited(ctx, 7, time.Now().Add(time.Minute)))
	require.NoError(t, repo.SetTempUnschedulable(ctx, 7, time.Now().Add(time.Minute), "probe"))
	require.NoError(t, repo.UpdateExtra(ctx, 7, map[string]any{"probe": true}))
	require.NoError(t, repo.UpdateCredentials(ctx, 7, map[string]any{"access_token": "new"}))
	cleared, err := repo.ClearRateLimitIfObserved(ctx, 7, time.Now(), time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.False(t, cleared)
	require.NoError(t, mock.ExpectationsWereMet())
}
