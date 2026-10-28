package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/requestmemory"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type streamRecoveryRedisTestCache struct {
	GatewayCache
	rdb *redis.Client
}

func (s *streamRecoveryRedisTestCache) StreamRecoveryRedisClient() redis.UniversalClient {
	return s.rdb
}
func (s *streamRecoveryRedisTestCache) GetSessionAccountID(ctx context.Context, g int64, key string) (int64, error) {
	id, err := s.rdb.Get(ctx, fmt.Sprintf("sticky_session:%d:%s", g, key)).Int64()
	if err == redis.Nil {
		return 0, ErrStickySessionNotFound
	}
	return id, err
}
func (s *streamRecoveryRedisTestCache) SetSessionAccountID(ctx context.Context, g int64, key string, id int64, ttl time.Duration) error {
	return s.rdb.Set(ctx, fmt.Sprintf("sticky_session:%d:%s", g, key), id, ttl).Err()
}
func (s *streamRecoveryRedisTestCache) DeleteSessionAccountID(ctx context.Context, g int64, key string) error {
	return s.rdb.Del(ctx, fmt.Sprintf("sticky_session:%d:%s", g, key)).Err()
}

func streamRecoveryTestContext(u, k, g int64) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: k, UserID: u, GroupID: &g})
	SetOpenAIHTTPResponseOwner(c, u, k)
	return c
}
func streamRecoveryTestService(t *testing.T, rdb *redis.Client) *OpenAIGatewayService {
	t.Helper()
	s := &OpenAIGatewayService{}
	if rdb != nil {
		s.cache = &streamRecoveryRedisTestCache{rdb: rdb}
	}
	t.Cleanup(func() { s.getStreamRecoveryContextStore().Close() })
	return s
}
func streamRecoveryTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

var streamRecoveryCompletedOne = json.RawMessage(`[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer one"}]}]`)

func TestStreamRecoveryContextResolveLegacyCompatibility(t *testing.T) {
	ctx := context.Background()
	var absent *OpenAIGatewayService
	for _, body := range []string{`{"input":"first"}`, `{"input":"first","previous_response_id":null}`, `{"input":"first","previous_response_id":""}`} {
		original := []byte(body)
		resolved, err := absent.ResolveStreamRecoveryContext(ctx, nil, original)
		require.NoError(t, err)
		require.Equal(t, original, resolved)
		require.Equal(t, &original[0], &resolved[0], "no-prev must return original bytes without a copy")
	}
	s := streamRecoveryTestService(t, nil)
	store := s.getOpenAIWSStateStore()
	require.NoError(t, store.BindHTTPResponseOwner(ctx, 3, "resp_normal", 1, 2, time.Hour))
	for _, c := range []*gin.Context{nil, streamRecoveryTestContext(1, 2, 3), streamRecoveryTestContext(1, 4, 3), streamRecoveryTestContext(1, 2, 0)} {
		original := []byte(`{"previous_response_id":"resp_normal","input":"normal next turn"}`)
		resolved, err := s.ResolveStreamRecoveryContext(ctx, c, original)
		require.NoError(t, err)
		require.Equal(t, original, resolved)
	}
	owned, err := s.ValidateOpenAIHTTPResponseOwner(ctx, 3, "resp_normal", 1, 4)
	require.NoError(t, err)
	require.True(t, owned, "legacy validation preserves same-user/different-key continuations")
}

func TestStreamRecoveryContextAliasAndMultiTurnProvenance(t *testing.T) {
	ctx := context.Background()
	s := streamRecoveryTestService(t, nil)
	c := streamRecoveryTestContext(1, 2, 3)
	first := []byte(`{"model":"test","input":"question one","stream":true}`)
	require.NoError(t, s.BindStreamRecoveryContext(ctx, c, "resp_sr_logical1", "resp_upstream1", &Account{ID: 101}, first, streamRecoveryCompletedOne))
	delta := []byte(`{"model":"test","previous_response_id":"resp_sr_logical1","input":"question two"}`)
	resolved, err := s.ResolveStreamRecoveryContext(ctx, c, delta)
	require.NoError(t, err)
	var root map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(resolved, &root))
	require.JSONEq(t, `"resp_upstream1"`, string(root["previous_response_id"]))
	require.JSONEq(t, `"question two"`, string(root["input"]))
	prepared, err := s.PrepareStreamRecoveryContext(ctx, c, resolved, 102)
	require.NoError(t, err)
	root = nil
	require.NoError(t, json.Unmarshal(prepared, &root))
	require.NotContains(t, root, "previous_response_id")
	require.JSONEq(t, `[{"type":"message","role":"user","content":"question one"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer one"}]},{"type":"message","role":"user","content":"question two"}]`, string(root["input"]))
	secondOutput := json.RawMessage(`[{"type":"message","role":"assistant","content":"answer two"}]`)
	require.NoError(t, s.BindStreamRecoveryContext(ctx, c, "resp_sr_logical2", "resp_upstream2", &Account{ID: 102}, resolved, secondOutput))
	third := []byte(`{"previous_response_id":"resp_sr_logical2","input":"question three"}`)
	prepared, err = s.PrepareStreamRecoveryContext(ctx, c, third, 103)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(prepared, &root))
	var items []json.RawMessage
	require.NoError(t, json.Unmarshal(root["input"], &items))
	require.Len(t, items, 5)
	require.Contains(t, string(items[0]), "question one")
	require.Contains(t, string(items[1]), "answer one")
	require.Contains(t, string(items[2]), "question two")
	require.Contains(t, string(items[3]), "answer two")
	require.Contains(t, string(items[4]), "question three")
	u, k, found, err := s.getOpenAIWSStateStore().GetHTTPResponseOwner(ctx, 3, "resp_upstream2")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, int64(1), u)
	require.Equal(t, int64(2), k)
	account, err := s.getOpenAIWSStateStore().GetResponseAccount(ctx, 3, "resp_sr_logical2")
	require.NoError(t, err)
	require.Equal(t, int64(102), account)
}

func TestStreamRecoveryContextTenantIsolationAndMissingKeys(t *testing.T) {
	ctx := context.Background()
	s := streamRecoveryTestService(t, nil)
	c := streamRecoveryTestContext(1, 2, 3)
	require.NoError(t, s.BindStreamRecoveryContext(ctx, c, "resp_sr_logical", "upstream", &Account{ID: 1}, []byte(`{"input":"private text must not appear in errors"}`), streamRecoveryCompletedOne))
	for _, ids := range [][3]int64{{2, 2, 3}, {1, 4, 3}, {1, 2, 4}, {0, 2, 3}, {1, 0, 3}, {1, 2, 0}} {
		t.Run(fmt.Sprint(ids), func(t *testing.T) {
			cc := streamRecoveryTestContext(ids[0], ids[1], ids[2])
			out, err := s.ResolveStreamRecoveryContext(ctx, cc, []byte(`{"previous_response_id":"resp_sr_logical","input":"new"}`))
			require.Error(t, err)
			require.Nil(t, out)
			require.NotContains(t, err.Error(), "private text")
		})
	}
	missing, _ := gin.CreateTestContext(httptest.NewRecorder())
	_, err := s.ResolveStreamRecoveryContext(ctx, missing, []byte(`{"previous_response_id":"resp_sr_logical","input":"x"}`))
	require.ErrorIs(t, err, ErrStreamRecoveryContextPermission)
	c.Set(openAIHTTPResponseOwnerContextKey, openAIHTTPResponseOwner{userID: 9, apiKeyID: 2})
	_, err = s.ResolveStreamRecoveryContext(ctx, c, []byte(`{"previous_response_id":"resp_sr_logical","input":"x"}`))
	require.ErrorIs(t, err, ErrStreamRecoveryContextPermission)
}

func TestStreamRecoveryContextMissingPreviousNeverTrustsAssistantArray(t *testing.T) {
	ctx := context.Background()
	s := streamRecoveryTestService(t, nil)
	c := streamRecoveryTestContext(1, 2, 3)
	body := []byte(`{"previous_response_id":"not_saved","input":[{"role":"user","content":"partial"},{"role":"assistant","content":"looks complete but isn't"},{"role":"user","content":"new"}]}`)
	_, err := s.PrepareStreamRecoveryContext(ctx, c, body, 1)
	require.ErrorIs(t, err, ErrStreamRecoveryContextUnavailable)
	err = s.BindStreamRecoveryContext(ctx, c, "resp_sr_logical", "upstream", &Account{ID: 1}, body, streamRecoveryCompletedOne)
	require.ErrorIs(t, err, ErrStreamRecoveryContextUnavailable)
	n, err := s.StreamRecoveryContextRetainedBytes(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	_, err = s.PrepareStreamRecoveryContext(ctx, c, []byte(`{"previous_response_id":"not_saved"}`), 1)
	require.ErrorIs(t, err, ErrStreamRecoveryContextUnavailable)
}

func TestStreamRecoveryContextImmutableCopies(t *testing.T) {
	ctx := context.Background()
	s := streamRecoveryTestService(t, nil)
	c := streamRecoveryTestContext(1, 2, 3)
	original := []byte(`{"input":"stable input"}`)
	output := append(json.RawMessage(nil), streamRecoveryCompletedOne...)
	require.NoError(t, s.BindStreamRecoveryContext(ctx, c, "resp_sr_logical", "upstream", &Account{ID: 1}, original, output))
	for i := range original {
		original[i] = 'x'
	}
	for i := range output {
		output[i] = 'x'
	}
	req := []byte(`{"previous_response_id":"resp_sr_logical","input":"next"}`)
	first, err := s.PrepareStreamRecoveryContext(ctx, c, req, 2)
	require.NoError(t, err)
	require.Contains(t, string(first), "stable input")
	require.Contains(t, string(first), "answer one")
	for i := range first {
		first[i] = 'x'
	}
	second, err := s.PrepareStreamRecoveryContext(ctx, c, req, 2)
	require.NoError(t, err)
	require.Contains(t, string(second), "stable input")
	require.JSONEq(t, `{"previous_response_id":"resp_sr_logical","input":"next"}`, string(req))
}

func TestStreamRecoveryContextOpaqueCannotMigrateAndSummaryIsPortable(t *testing.T) {
	ctx := context.Background()
	c := streamRecoveryTestContext(1, 2, 3)
	for _, opaque := range []string{
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"public summary"}],"encrypted_content":"opaque ciphertext"}`,
		`{"type":"compaction","encrypted_content":"opaque compacted history"}`,
		`{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"opaque signature"}]}`,
	} {
		t.Run(opaque[:20], func(t *testing.T) {
			s := streamRecoveryTestService(t, nil)
			out := json.RawMessage("[" + opaque + "]")
			require.NoError(t, s.BindStreamRecoveryContext(ctx, c, "resp_sr_logical", "upstream", &Account{ID: 101}, []byte(`{"input":"original"}`), out))
			req := []byte(`{"previous_response_id":"resp_sr_logical","input":"next"}`)
			_, err := s.PrepareStreamRecoveryContext(ctx, c, req, 102)
			require.ErrorIs(t, err, ErrStreamRecoveryContextUnavailable)
			body, err := s.PrepareStreamRecoveryContext(ctx, c, req, 101)
			require.NoError(t, err)
			require.Contains(t, string(body), "opaque")
			_, err = s.PrepareStreamRecoveryContext(ctx, c, req, 0)
			require.ErrorIs(t, err, ErrStreamRecoveryContextUnavailable)
		})
	}
	s := streamRecoveryTestService(t, nil)
	require.NoError(t, s.BindStreamRecoveryContext(ctx, c, "resp_sr_logical", "upstream", &Account{ID: 101}, []byte(`{"input":"original"}`), json.RawMessage(`[{"type":"reasoning","summary":[{"type":"summary_text","text":"public summary"}]}]`)))
	body, err := s.PrepareStreamRecoveryContext(ctx, c, []byte(`{"previous_response_id":"resp_sr_logical","input":"next"}`), 102)
	require.NoError(t, err)
	require.Contains(t, string(body), "public summary")
	_, err = s.PrepareStreamRecoveryContext(ctx, c, []byte(`{"input":[{"type":"compaction","encrypted_content":"unknown source"}]}`), 101)
	require.ErrorIs(t, err, ErrStreamRecoveryContextUnavailable)
}

func TestStreamRecoveryContextLocalTTLAndRestartFailClosed(t *testing.T) {
	ctx := context.Background()
	s := streamRecoveryTestService(t, nil)
	c := streamRecoveryTestContext(1, 2, 3)
	require.NoError(t, s.BindStreamRecoveryContext(ctx, c, "resp_sr_logical", "upstream", &Account{ID: 1}, []byte(`{"input":"x"}`), streamRecoveryCompletedOne))
	store := s.getStreamRecoveryContextStore()
	now := time.Now().Add(StreamRecoveryContextTTL + time.Minute)
	store.now = func() time.Time { return now }
	_, err := s.ResolveStreamRecoveryContext(ctx, c, []byte(`{"previous_response_id":"resp_sr_logical","input":"next"}`))
	require.ErrorIs(t, err, ErrStreamRecoveryContextUnavailable)
	n, err := s.StreamRecoveryContextRetainedBytes(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	// Ownership can survive a process restart, but fallback history cannot.
	restarted := streamRecoveryTestService(t, nil)
	restarted.openaiWSStateStore = s.getOpenAIWSStateStore()
	_, err = restarted.ResolveStreamRecoveryContext(ctx, c, []byte(`{"previous_response_id":"resp_sr_logical","input":"next"}`))
	require.ErrorIs(t, err, ErrStreamRecoveryContextUnavailable)
}

func TestStreamRecoveryContextRedisCrossInstanceAliasAndTTL(t *testing.T) {
	mr, rdb := streamRecoveryTestRedis(t)
	ctx := context.Background()
	c := streamRecoveryTestContext(1, 2, 3)
	writer := streamRecoveryTestService(t, rdb)
	reader := streamRecoveryTestService(t, rdb)
	require.NoError(t, writer.BindStreamRecoveryContext(ctx, c, "resp_sr_logical", "upstream", &Account{ID: 77}, []byte(`{"input":"first"}`), streamRecoveryCompletedOne))
	body, err := reader.ResolveStreamRecoveryContext(ctx, c, []byte(`{"previous_response_id":"resp_sr_logical","input":"next"}`))
	require.NoError(t, err)
	require.Contains(t, string(body), `"previous_response_id":"upstream"`)
	body, err = reader.PrepareStreamRecoveryContext(ctx, c, body, 78)
	require.NoError(t, err)
	require.Contains(t, string(body), "first")
	require.Contains(t, string(body), "answer one")
	bytes, err := reader.StreamRecoveryContextRetainedBytes(ctx)
	require.NoError(t, err)
	require.Positive(t, bytes)
	require.LessOrEqual(t, bytes, StreamRecoveryContextMaxBytes)
	for _, key := range streamRecoveryRedisKeys {
		require.LessOrEqual(t, mr.TTL(key), StreamRecoveryContextTTL)
	}
	mr.FastForward(StreamRecoveryContextTTL + time.Second)
	_, err = reader.ResolveStreamRecoveryContext(ctx, c, []byte(`{"previous_response_id":"resp_sr_logical","input":"next"}`))
	require.ErrorIs(t, err, ErrStreamRecoveryContextUnavailable)
	bytes, err = reader.StreamRecoveryContextRetainedBytes(ctx)
	require.NoError(t, err)
	require.Zero(t, bytes)
}

func TestStreamRecoveryContextRedisBytesEntriesAndEviction(t *testing.T) {
	_, rdb := streamRecoveryTestRedis(t)
	ctx := context.Background()
	s := streamRecoveryTestService(t, rdb)
	c := streamRecoveryTestContext(1, 2, 3)
	// Real Redis scripts exercise global byte eviction, then entry eviction.
	large, _ := json.Marshal(map[string]any{"input": strings.Repeat("x", (6 << 20))})
	for i := 0; i < 3; i++ {
		require.NoError(t, s.BindStreamRecoveryContext(ctx, c, fmt.Sprintf("resp_sr_large%d", i), fmt.Sprintf("up%d", i), &Account{ID: 1}, large, streamRecoveryCompletedOne))
	}
	bytes, err := s.StreamRecoveryContextRetainedBytes(ctx)
	require.NoError(t, err)
	require.LessOrEqual(t, bytes, StreamRecoveryContextMaxBytes)
	_, err = s.ResolveStreamRecoveryContext(ctx, c, []byte(`{"previous_response_id":"resp_sr_large0","input":"next"}`))
	require.ErrorIs(t, err, ErrStreamRecoveryContextUnavailable)
	tooLarge, _ := json.Marshal(map[string]any{"input": strings.Repeat("x", (8 << 20))})
	err = s.BindStreamRecoveryContext(ctx, c, "resp_sr_tooLarge", "tooLargeUp", &Account{ID: 1}, tooLarge, streamRecoveryCompletedOne)
	require.NoError(t, err, "oversized original must still bind its ID alias")
	aliased, err := s.ResolveStreamRecoveryContext(ctx, c, []byte(`{"previous_response_id":"resp_sr_tooLarge","input":"next"}`))
	require.NoError(t, err)
	require.Contains(t, string(aliased), `"previous_response_id":"tooLargeUp"`)
	_, err = s.PrepareStreamRecoveryContext(ctx, c, aliased, 1)
	require.ErrorIs(t, err, ErrStreamRecoveryContextUnavailable)
	require.NoError(t, rdb.Del(ctx, streamRecoveryRedisKeys...).Err())
	for i := 0; i < StreamRecoveryContextMaxEntries+1; i++ {
		require.NoError(t, s.BindStreamRecoveryContext(ctx, c, fmt.Sprintf("small%04d", i), fmt.Sprintf("sup%04d", i), &Account{ID: 1}, []byte(`{"input":"x"}`), streamRecoveryCompletedOne))
	}
	n, err := rdb.HLen(ctx, streamRecoveryRedisKeys[0]).Result()
	require.NoError(t, err)
	require.Equal(t, int64(StreamRecoveryContextMaxEntries), n)
	a, err := rdb.HLen(ctx, streamRecoveryRedisKeys[1]).Result()
	require.NoError(t, err)
	require.Equal(t, int64(2*StreamRecoveryContextMaxEntries), a)
}

func TestStreamRecoveryContextRedisConcurrentGlobalBudget(t *testing.T) {
	_, rdb := streamRecoveryTestRedis(t)
	ctx := context.Background()
	c := streamRecoveryTestContext(1, 2, 3)
	one, two := streamRecoveryTestService(t, rdb), streamRecoveryTestService(t, rdb)
	large, _ := json.Marshal(map[string]any{"input": strings.Repeat("x", (3 << 20))})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := one
			if i%2 != 0 {
				s = two
			}
			errs <- s.BindStreamRecoveryContext(ctx, c, fmt.Sprintf("con%d", i), fmt.Sprintf("conup%d", i), &Account{ID: 1}, large, streamRecoveryCompletedOne)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	bytes, err := two.StreamRecoveryContextRetainedBytes(ctx)
	require.NoError(t, err)
	require.LessOrEqual(t, bytes, StreamRecoveryContextMaxBytes)
	n, err := rdb.HLen(ctx, streamRecoveryRedisKeys[0]).Result()
	require.NoError(t, err)
	require.LessOrEqual(t, n, int64(5))
}

func TestStreamRecoveryContextLargeOriginalBindsAliasOnly(t *testing.T) {
	for _, useRedis := range []bool{false, true} {
		t.Run(fmt.Sprint(useRedis), func(t *testing.T) {
			var rdb *redis.Client
			if useRedis {
				_, rdb = streamRecoveryTestRedis(t)
			}
			s := streamRecoveryTestService(t, rdb)
			c := streamRecoveryTestContext(1, 2, 3)
			manager, err := requestmemory.New(64<<10, 0, 0)
			require.NoError(t, err)
			lease, err := manager.Acquire(context.Background(), 1)
			require.NoError(t, err)
			defer lease.Release()
			ctx := requestmemory.WithLease(context.Background(), lease)
			original, err := json.Marshal(map[string]any{"input": strings.Repeat("x", 9<<20)})
			require.NoError(t, err)
			require.NoError(t, s.BindStreamRecoveryContext(ctx, c, "resp_sr_large_alias", "resp_large_upstream", &Account{ID: 99}, original, streamRecoveryCompletedOne))
			resolved, err := s.ResolveStreamRecoveryContext(ctx, c, []byte(`{"previous_response_id":"resp_sr_large_alias","input":"next"}`))
			require.NoError(t, err)
			require.Contains(t, string(resolved), `"previous_response_id":"resp_large_upstream"`)
			_, err = s.PrepareStreamRecoveryContext(ctx, c, resolved, 99)
			require.ErrorIs(t, err, ErrStreamRecoveryContextUnavailable)
			require.Contains(t, err.Error(), "no complete replay provenance")
			require.Less(t, manager.Snapshot().UsedBytes, int64(64<<10), "metadata-only operation must not charge or clone the large request")
			retained, err := s.StreamRecoveryContextRetainedBytes(ctx)
			require.NoError(t, err)
			require.Less(t, retained, int64(4096))
			// A following normal turn is still aliased, but never invents the
			// unavailable prefix history from its delta.
			require.NoError(t, s.BindStreamRecoveryContext(context.Background(), c, "resp_sr_large_next", "resp_large_next_up", &Account{ID: 99}, resolved, streamRecoveryCompletedOne))
			_, err = s.PrepareStreamRecoveryContext(context.Background(), c, []byte(`{"previous_response_id":"resp_sr_large_next","input":"third"}`), 99)
			require.ErrorIs(t, err, ErrStreamRecoveryContextUnavailable)
		})
	}
}

func TestStreamRecoveryContextRedisReadsChargeActualLengthAndRejectGrowth(t *testing.T) {
	_, rdb := streamRecoveryTestRedis(t)
	s := streamRecoveryTestService(t, rdb)
	c := streamRecoveryTestContext(1, 2, 3)
	require.NoError(t, s.BindStreamRecoveryContext(context.Background(), c, "resp_sr_small", "resp_small_up", &Account{ID: 1}, []byte(`{"input":"small"}`), streamRecoveryCompletedOne))
	manager, err := requestmemory.New(128<<10, 0, 0)
	require.NoError(t, err)
	lease, err := manager.Acquire(context.Background(), 1)
	require.NoError(t, err)
	defer lease.Release()
	ctx := requestmemory.WithLease(context.Background(), lease)
	for i := 0; i < 3; i++ {
		resolved, err := s.ResolveStreamRecoveryContext(ctx, c, []byte(`{"previous_response_id":"resp_sr_small","input":"next"}`))
		require.NoError(t, err)
		require.Contains(t, string(resolved), `"previous_response_id":"resp_small_up"`)
	}
	require.Less(t, manager.Snapshot().UsedBytes, int64(64<<10), "three small reads must not reserve 288 MiB")
	key := streamRecoveryContextKey(streamRecoveryContextOwner{1, 2, 3}, "resp_sr_small")
	length, err := streamRecoveryRedisLength.Run(ctx, rdb, streamRecoveryRedisKeys, key).Int64()
	require.NoError(t, err)
	payload, err := rdb.HGet(ctx, streamRecoveryRedisKeys[0], key).Result()
	require.NoError(t, err)
	require.NoError(t, rdb.HSet(ctx, streamRecoveryRedisKeys[0], key, payload+strings.Repeat(" ", 1024)).Err())
	_, err = streamRecoveryRedisGet.Run(ctx, rdb, streamRecoveryRedisKeys, key, length).Text()
	require.Error(t, err, "a growing entry must be rejected before the uncharged bytes are returned")
}

func TestStreamRecoveryContextLocalRecordBudgetAndOwnerCollision(t *testing.T) {
	ctx := context.Background()
	s := streamRecoveryTestService(t, nil)
	c := streamRecoveryTestContext(1, 2, 3)
	for i := 0; i < StreamRecoveryContextMaxEntries+1; i++ {
		require.NoError(t, s.BindStreamRecoveryContext(ctx, c, fmt.Sprintf("resp_sr_local%04d", i), fmt.Sprintf("localup%04d", i), &Account{ID: 1}, []byte(`{"input":"small"}`), streamRecoveryCompletedOne))
	}
	store := s.getStreamRecoveryContextStore()
	require.Len(t, store.entries, StreamRecoveryContextMaxEntries)
	require.Len(t, store.aliases, 2*StreamRecoveryContextMaxEntries)
	retained, err := store.RetainedBytes(ctx)
	require.NoError(t, err)
	require.LessOrEqual(t, retained, StreamRecoveryContextMaxBytes)
	_, err = s.ResolveStreamRecoveryContext(ctx, c, []byte(`{"previous_response_id":"resp_sr_local0000","input":"next"}`))
	require.ErrorIs(t, err, ErrStreamRecoveryContextUnavailable)
	ws := s.getOpenAIWSStateStore()
	require.NoError(t, ws.BindHTTPResponseOwner(ctx, 3, "foreign_up", 9, 10, time.Hour))
	err = s.BindStreamRecoveryContext(ctx, c, "resp_sr_collision", "foreign_up", &Account{ID: 1}, []byte(`{"input":"small"}`), streamRecoveryCompletedOne)
	require.ErrorIs(t, err, ErrStreamRecoveryContextPermission)
	u, k, found, err := ws.GetHTTPResponseOwner(ctx, 3, "foreign_up")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, int64(9), u)
	require.Equal(t, int64(10), k)
}
