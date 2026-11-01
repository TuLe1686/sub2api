package service

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/requestmemory"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	StreamRecoveryContextMaxBytes      int64 = 16 << 20
	StreamRecoveryContextMaxEntryBytes int64 = 8 << 20
	StreamRecoveryContextMaxEntries          = 1024
	StreamRecoveryContextTTL                 = 30 * time.Minute
	streamRecoveryContextOverhead      int64 = 512
)

var ErrStreamRecoveryContextPermission = errors.New("stream recovery context owner mismatch")

// StreamRecoveryRedisProvider is optional; no database schema or cache interface migration.
type StreamRecoveryRedisProvider interface{ StreamRecoveryRedisClient() redis.UniversalClient }

type streamRecoveryContextOwner struct{ UserID, APIKeyID, GroupID int64 }
type streamRecoveryContextEntry struct {
	Owner                       streamRecoveryContextOwner `json:"owner"`
	LogicalID                   string                     `json:"logical_id"`
	UpstreamID                  string                     `json:"upstream_id"`
	AccountID                   int64                      `json:"account_id"`
	History                     json.RawMessage            `json:"history"`
	AccountBound                bool                       `json:"account_bound"`
	ProvenanceUnavailableReason string                     `json:"provenance_unavailable_reason,omitempty"`
}
type streamRecoveryLocalEntry struct {
	entry   streamRecoveryContextEntry
	alias   string
	bytes   int64
	expires time.Time
	lru     *list.Element
}

// Only the fallback retains history in this process. Charge includes fixed metadata
// allowance; multiple stores share this process budget, rather than each getting 16 MiB.
var streamRecoveryProcessRetained atomic.Int64

// StreamRecoveryContextStore stores only authenticated, gateway-proven Responses
// histories. Redis is authoritative when available: an error never falls back to
// a divergent local history. Both IDs alias one bounded record.
type StreamRecoveryContextStore struct {
	ws       OpenAIWSStateStore
	rdb      redis.UniversalClient
	mu       sync.Mutex
	entries  map[string]*streamRecoveryLocalEntry
	aliases  map[string]string
	lru      list.List
	retained int64
	now      func() time.Time
}

func NewStreamRecoveryContextStore(ws OpenAIWSStateStore, rdb redis.UniversalClient) *StreamRecoveryContextStore {
	return &StreamRecoveryContextStore{ws: ws, rdb: rdb, entries: make(map[string]*streamRecoveryLocalEntry), aliases: make(map[string]string), now: time.Now}
}

func (s *OpenAIGatewayService) getStreamRecoveryContextStore() *StreamRecoveryContextStore {
	if s == nil {
		return nil
	}
	s.streamRecoveryContextOnce.Do(func() {
		if s.streamRecoveryContextStore == nil {
			var rdb redis.UniversalClient
			if provider, ok := s.cache.(StreamRecoveryRedisProvider); ok {
				rdb = provider.StreamRecoveryRedisClient()
			}
			s.streamRecoveryContextStore = NewStreamRecoveryContextStore(s.getOpenAIWSStateStore(), rdb)
		}
	})
	return s.streamRecoveryContextStore
}

func streamRecoveryAuthenticatedOwner(c *gin.Context) (streamRecoveryContextOwner, error) {
	if c == nil {
		return streamRecoveryContextOwner{}, ErrStreamRecoveryContextPermission
	}
	v, ok := c.Get("api_key")
	key, valid := v.(*APIKey)
	if !ok || !valid || key == nil || key.ID <= 0 || key.UserID <= 0 || key.GroupID == nil || *key.GroupID <= 0 {
		return streamRecoveryContextOwner{}, ErrStreamRecoveryContextPermission
	}
	owner := streamRecoveryContextOwner{key.UserID, key.ID, *key.GroupID}
	if v, exists := c.Get(openAIHTTPResponseOwnerContextKey); exists {
		marked, ok := v.(openAIHTTPResponseOwner)
		if !ok || marked.userID != owner.UserID || marked.apiKeyID != owner.APIKeyID {
			return streamRecoveryContextOwner{}, ErrStreamRecoveryContextPermission
		}
	}
	return owner, nil
}

func streamRecoveryContextKey(owner streamRecoveryContextOwner, id string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%d/%d/%s", owner.UserID, owner.APIKeyID, owner.GroupID, id)))
	return hex.EncodeToString(sum[:])
}

func streamRecoveryContextID(id string) bool {
	return id != "" && len(id) <= 512 && id == normalizeOpenAIWSResponseID(id)
}

func streamRecoveryContextRequest(body []byte) (map[string]json.RawMessage, string, error) {
	if int64(len(body)) > StreamRecoveryContextMaxEntryBytes {
		return nil, "", streamRecoveryContextError("request exceeds history entry byte budget")
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return nil, "", streamRecoveryContextError("invalid request object")
	}
	var previous string
	if raw, ok := root["previous_response_id"]; ok && string(raw) != "null" {
		if json.Unmarshal(raw, &previous) != nil || (previous != "" && !streamRecoveryContextID(previous)) {
			return nil, "", streamRecoveryContextError("invalid previous response identifier")
		}
	}
	for _, name := range []string{"conversation", "conversation_id", "turn_state", "x-codex-turn-state"} {
		if raw, ok := root[name]; ok && string(raw) != "null" && string(raw) != `""` {
			return nil, "", streamRecoveryContextError("external conversation provenance unavailable")
		}
	}
	return root, previous, nil
}

func streamRecoveryInlineItems(raw json.RawMessage, output bool) ([]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, streamRecoveryContextError("inline input or completed output missing")
	}
	var items []json.RawMessage
	if raw[0] == '"' && !output {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return nil, streamRecoveryContextError("invalid inline string")
		}
		item, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": text})
		return []json.RawMessage{item}, nil
	}
	if json.Unmarshal(raw, &items) != nil || items == nil {
		return nil, streamRecoveryContextError("inline history must be an array or string")
	}
	for _, item := range items {
		if !gjson.ValidBytes(item) || !gjson.ParseBytes(item).IsObject() {
			return nil, streamRecoveryContextError("history item must be an object")
		}
	}
	return items, nil
}

// Scan structure only. Never strip opaque blocks, signatures or compaction and
// then describe the remainder as complete. Plain reasoning summaries are portable.
func streamRecoveryHasAccountBoundValue(raw json.RawMessage) bool {
	if !gjson.ValidBytes(raw) {
		return true
	}
	var scan func(gjson.Result) bool
	scan = func(obj gjson.Result) bool {
		if !obj.IsObject() && !obj.IsArray() {
			return false
		}
		if obj.IsObject() {
			for _, name := range []string{"encrypted_content", "signature"} {
				if obj.Get(name).Exists() {
					return true
				}
			}
			switch obj.Get("type").String() {
			case "compaction", "item_reference", "redacted_thinking":
				return true
			}
		}
		found := false
		obj.ForEach(func(_, child gjson.Result) bool { found = scan(child); return !found })
		return found
	}
	return scan(gjson.ParseBytes(raw))
}

func (s *StreamRecoveryContextStore) authorized(ctx context.Context, owner streamRecoveryContextOwner, id string) error {
	if s == nil || s.ws == nil {
		return streamRecoveryContextError("owner store unavailable")
	}
	u, k, found, err := s.ws.GetHTTPResponseOwner(ctx, owner.GroupID, id)
	if err != nil {
		return streamRecoveryContextError("owner lookup unavailable")
	}
	if !found {
		return streamRecoveryContextError("previous response provenance missing or expired")
	}
	if u != owner.UserID || k != owner.APIKeyID {
		return ErrStreamRecoveryContextPermission
	}
	return nil
}

func (s *StreamRecoveryContextStore) lookup(ctx context.Context, owner streamRecoveryContextOwner, id string) (streamRecoveryContextEntry, error) {
	// The permission check deliberately precedes Redis history reads or alias rewrites.
	if err := s.authorized(ctx, owner, id); err != nil {
		return streamRecoveryContextEntry{}, err
	}
	key := streamRecoveryContextKey(owner, id)
	var entry streamRecoveryContextEntry
	if s.rdb != nil {
		cacheCtx, cancel := withOpenAIWSStateStoreRedisTimeout(ctx)
		defer cancel()
		length, err := streamRecoveryRedisLength.Run(cacheCtx, s.rdb, streamRecoveryRedisKeys, key).Int64()
		if err != nil {
			return entry, streamRecoveryContextError("history metadata unavailable")
		}
		if length <= 0 {
			return entry, streamRecoveryContextError("previous response provenance missing or expired")
		}
		if length > StreamRecoveryContextMaxEntryBytes {
			return entry, streamRecoveryContextError("history entry exceeds byte budget")
		}
		if err := requestmemory.FromContext(ctx).Charge(length*12 + 4096); err != nil {
			return entry, streamRecoveryContextError("request memory budget exceeded")
		}
		// The second Lua read checks length before returning bytes. An entry
		// growing between the two calls cannot bypass the pre-allocation charge.
		payload, err := streamRecoveryRedisGet.Run(cacheCtx, s.rdb, streamRecoveryRedisKeys, key, length).Text()
		if errors.Is(err, redis.Nil) {
			return entry, streamRecoveryContextError("previous response provenance missing or expired")
		}
		if err != nil || json.Unmarshal([]byte(payload), &entry) != nil {
			return entry, streamRecoveryContextError("history store unavailable")
		}
	} else {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.cleanupLocked()
		canonical, ok := s.aliases[key]
		if !ok {
			return entry, streamRecoveryContextError("previous response provenance missing or expired")
		}
		stored := s.entries[canonical]
		s.lru.MoveToBack(stored.lru)
		entry = stored.entry
		if err := requestmemory.FromContext(ctx).Charge(int64(len(entry.History)) * 12); err != nil {
			return streamRecoveryContextEntry{}, streamRecoveryContextError("request memory budget exceeded")
		}
		entry.History = append(json.RawMessage(nil), entry.History...)
	}
	if entry.Owner != owner || (id != entry.LogicalID && id != entry.UpstreamID) {
		return streamRecoveryContextEntry{}, ErrStreamRecoveryContextPermission
	}
	return entry, nil
}

// ResolveStreamRecoveryContext runs before the legacy Responses owner validator.
// Only the reserved resp_sr_ logical-ID namespace mandates recovery provenance.
// Ordinary upstream IDs without a usable recovery alias pass through unchanged,
// leaving legacy authorization (including same-user/different-key) to the caller.
// This method does not authorize those pass-through requests itself.
func (s *OpenAIGatewayService) ResolveStreamRecoveryContext(ctx context.Context, c *gin.Context, body []byte) ([]byte, error) {
	previousValue := gjson.GetBytes(body, "previous_response_id")
	if !previousValue.Exists() || previousValue.Type == gjson.Null || previousValue.String() == "" {
		return body, nil
	}
	previousID := previousValue.String()
	mandatory := previousValue.Type == gjson.String && strings.HasPrefix(previousID, "resp_sr_")
	if !gjson.ValidBytes(body) || previousValue.Type != gjson.String || !streamRecoveryContextID(previousID) {
		if !mandatory {
			return body, nil
		}
		return nil, streamRecoveryContextError("invalid previous response identifier")
	}
	owner, err := streamRecoveryAuthenticatedOwner(c)
	if err != nil {
		if !mandatory {
			return body, nil
		}
		return nil, err
	}
	entry, err := s.getStreamRecoveryContextStore().lookup(ctx, owner, previousID)
	if err != nil {
		if !mandatory {
			return body, nil
		}
		return nil, err
	}
	if entry.UpstreamID == previousID {
		return body, nil
	}
	if err := requestmemory.FromContext(ctx).Charge(int64(len(body))*2 + 1024); err != nil {
		return nil, streamRecoveryContextError("request memory budget exceeded")
	}
	return sjson.SetBytes(body, "previous_response_id", entry.UpstreamID)
}

// PrepareStreamRecoveryContext materializes a verified full history for an
// in-request recovery attempt. Target zero means unknown, never safe for opaque
// account-bound state. A same-account replay keeps opaque bytes intact.
func (s *OpenAIGatewayService) PrepareStreamRecoveryContext(ctx context.Context, c *gin.Context, body []byte, targetAccountID int64) ([]byte, error) {
	owner, err := streamRecoveryAuthenticatedOwner(c)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > StreamRecoveryContextMaxEntryBytes {
		return nil, streamRecoveryContextError("request exceeds history entry byte budget")
	}
	if err := requestmemory.FromContext(ctx).Charge(int64(len(body))*12 + 4096); err != nil {
		return nil, streamRecoveryContextError("request memory budget exceeded")
	}
	root, previous, err := streamRecoveryContextRequest(body)
	if err != nil {
		return nil, err
	}
	current, err := streamRecoveryInlineItems(root["input"], false)
	if err != nil {
		return nil, err
	}
	items := current
	origin := int64(0)
	if previous != "" {
		entry, err := s.getStreamRecoveryContextStore().lookup(ctx, owner, previous)
		if err != nil {
			return nil, err
		}
		if entry.ProvenanceUnavailableReason != "" || len(entry.History) == 0 || string(entry.History) == "null" {
			return nil, streamRecoveryContextError("previous response has no complete replay provenance")
		}
		var prior []json.RawMessage
		if json.Unmarshal(entry.History, &prior) != nil {
			return nil, streamRecoveryContextError("saved history invalid")
		}
		items = append(prior, current...)
		origin = entry.AccountID
	}
	history, err := json.Marshal(items)
	if err != nil {
		return nil, streamRecoveryContextError("history serialization failed")
	}
	if int64(len(history)) > StreamRecoveryContextMaxEntryBytes {
		return nil, streamRecoveryContextError("replay exceeds entry byte budget")
	}
	if streamRecoveryHasAccountBoundValue(history) && (origin <= 0 || targetAccountID != origin) {
		return nil, streamRecoveryContextError("opaque history cannot migrate accounts")
	}
	root["input"] = history
	delete(root, "previous_response_id")
	return json.Marshal(root)
}

// BindStreamRecoveryContext must run on EVERY successful recovery-enabled
// Responses request, before downstream completion, with complete logical output
// (not the first-attempt partial output). original is the accepted request, not
// an internally appended continuation prompt.
func (s *OpenAIGatewayService) BindStreamRecoveryContext(ctx context.Context, c *gin.Context, logicalID, upstreamID string, account *Account, original []byte, output json.RawMessage) error {
	owner, err := streamRecoveryAuthenticatedOwner(c)
	if err != nil {
		return err
	}
	if !streamRecoveryContextID(logicalID) || !streamRecoveryContextID(upstreamID) || account == nil || account.ID <= 0 {
		return streamRecoveryContextError("invalid successful response binding")
	}
	store := s.getStreamRecoveryContextStore()
	if store == nil || store.ws == nil {
		return streamRecoveryContextError("context store unavailable")
	}
	entry := streamRecoveryContextEntry{Owner: owner, LogicalID: logicalID, UpstreamID: upstreamID, AccountID: account.ID}
	if int64(len(original))+int64(len(output)) > StreamRecoveryContextMaxEntryBytes {
		// Successful large-image requests still need a durable ID alias. Do not
		// parse or clone their input merely to store bounded metadata.
		entry.ProvenanceUnavailableReason = "history_byte_budget"
		if err := requestmemory.FromContext(ctx).Charge(4096); err != nil {
			return streamRecoveryContextError("request memory budget exceeded")
		}
	} else {
		if err := requestmemory.FromContext(ctx).Charge((int64(len(original))+int64(len(output)))*12 + 4096); err != nil {
			return streamRecoveryContextError("request memory budget exceeded")
		}
		root, previous, err := streamRecoveryContextRequest(original)
		if err != nil {
			return err
		}
		current, err := streamRecoveryInlineItems(root["input"], false)
		if err != nil {
			return err
		}
		completed, err := streamRecoveryInlineItems(output, true)
		if err != nil {
			return err
		}
		items := current
		if previous != "" {
			prior, err := store.lookup(ctx, owner, previous)
			if err != nil {
				return err
			}
			if prior.ProvenanceUnavailableReason != "" || len(prior.History) == 0 || string(prior.History) == "null" {
				entry.ProvenanceUnavailableReason = "previous_history_unavailable"
			} else {
				var history []json.RawMessage
				if json.Unmarshal(prior.History, &history) != nil {
					return streamRecoveryContextError("saved history invalid")
				}
				if prior.AccountBound && prior.AccountID != account.ID {
					return streamRecoveryContextError("opaque history cannot migrate accounts")
				}
				items = append(history, current...)
			}
		}
		if entry.ProvenanceUnavailableReason == "" {
			items = append(items, completed...)
			history, err := json.Marshal(items)
			if err != nil {
				return streamRecoveryContextError("history serialization failed")
			}
			if int64(len(history))+streamRecoveryContextOverhead+2048 > StreamRecoveryContextMaxEntryBytes {
				entry.ProvenanceUnavailableReason = "history_byte_budget"
			} else {
				entry.History = history
				entry.AccountBound = streamRecoveryHasAccountBoundValue(history)
			}
		}
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		return streamRecoveryContextError("context serialization failed")
	}
	if int64(len(payload))+streamRecoveryContextOverhead > StreamRecoveryContextMaxEntryBytes {
		entry.History = nil
		entry.AccountBound = false
		entry.ProvenanceUnavailableReason = "history_byte_budget"
		payload, err = json.Marshal(entry)
		if err != nil {
			return streamRecoveryContextError("context serialization failed")
		}
	}
	// Check collisions before creating either binding; never overwrite another tenant.
	for _, id := range []string{logicalID, upstreamID} {
		u, k, found, err := store.ws.GetHTTPResponseOwner(ctx, owner.GroupID, id)
		if err != nil && !errors.Is(err, ErrStickySessionNotFound) {
			return streamRecoveryContextError("owner lookup unavailable")
		}
		if found && (u != owner.UserID || k != owner.APIKeyID) {
			return ErrStreamRecoveryContextPermission
		}
	}
	bindCtx, cancel := withOpenAIWSStateStoreRedisTimeout(ctx)
	defer cancel()
	for _, id := range []string{logicalID, upstreamID} {
		if err := store.ws.BindHTTPResponseOwner(bindCtx, owner.GroupID, id, owner.UserID, owner.APIKeyID, StreamRecoveryContextTTL); err != nil {
			return streamRecoveryContextError("owner binding unavailable")
		}
		if err := store.ws.BindResponseAccount(bindCtx, owner.GroupID, id, account.ID, StreamRecoveryContextTTL); err != nil {
			return streamRecoveryContextError("account binding unavailable")
		}
	}
	return store.put(bindCtx, entry, payload)
}

func (s *StreamRecoveryContextStore) put(ctx context.Context, entry streamRecoveryContextEntry, payload []byte) error {
	canonical, alias := streamRecoveryContextKey(entry.Owner, entry.LogicalID), streamRecoveryContextKey(entry.Owner, entry.UpstreamID)
	charge := int64(len(payload)) + streamRecoveryContextOverhead
	if charge > StreamRecoveryContextMaxEntryBytes {
		return streamRecoveryContextError("context exceeds entry byte budget")
	}
	if s.rdb != nil {
		n, err := streamRecoveryRedisPut.Run(ctx, s.rdb, streamRecoveryRedisKeys, canonical, alias, string(payload), charge, StreamRecoveryContextTTL.Milliseconds(), StreamRecoveryContextMaxBytes, StreamRecoveryContextMaxEntries).Int()
		if err != nil || n != 1 {
			return streamRecoveryContextError("history binding unavailable or capacity exceeded")
		}
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked()
	if existing := s.aliases[alias]; existing != "" && existing != canonical {
		return streamRecoveryContextError("response alias conflict")
	}
	if existing := s.aliases[canonical]; existing != "" && existing != canonical {
		return streamRecoveryContextError("response alias conflict")
	}
	if old := s.entries[canonical]; old != nil {
		s.removeLocked(canonical)
	}
	for len(s.entries) >= StreamRecoveryContextMaxEntries || s.retained+charge > StreamRecoveryContextMaxBytes {
		s.removeLocked(s.lru.Front().Value.(string))
	}
	for {
		used := streamRecoveryProcessRetained.Load()
		if used+charge > StreamRecoveryContextMaxBytes {
			if s.lru.Len() == 0 {
				return streamRecoveryContextError("process history byte budget exceeded")
			}
			s.removeLocked(s.lru.Front().Value.(string))
			continue
		}
		if streamRecoveryProcessRetained.CompareAndSwap(used, used+charge) {
			break
		}
	}
	entry.History = append(json.RawMessage(nil), entry.History...)
	s.entries[canonical] = &streamRecoveryLocalEntry{entry: entry, alias: alias, bytes: charge, expires: s.now().Add(StreamRecoveryContextTTL), lru: s.lru.PushBack(canonical)}
	s.aliases[canonical], s.aliases[alias] = canonical, canonical
	s.retained += charge
	return nil
}

func (s *StreamRecoveryContextStore) removeLocked(key string) {
	entry := s.entries[key]
	if entry == nil {
		return
	}
	delete(s.entries, key)
	delete(s.aliases, key)
	delete(s.aliases, entry.alias)
	s.lru.Remove(entry.lru)
	s.retained -= entry.bytes
	streamRecoveryProcessRetained.Add(-entry.bytes)
}
func (s *StreamRecoveryContextStore) cleanupLocked() {
	now := s.now()
	for key, entry := range s.entries {
		if !now.Before(entry.expires) {
			s.removeLocked(key)
		}
	}
}

// Close releases local fallback history and its shared process budget.
func (s *StreamRecoveryContextStore) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.entries {
		s.removeLocked(key)
	}
}

// RetainedBytes includes charged metadata; Redis is bounded across ALL instances
// sharing its namespace, while local fallback uses the shared process limit.
func (s *StreamRecoveryContextStore) RetainedBytes(ctx context.Context) (int64, error) {
	if s == nil {
		return 0, nil
	}
	if s.rdb != nil {
		cacheCtx, cancel := withOpenAIWSStateStoreRedisTimeout(ctx)
		defer cancel()
		n, err := streamRecoveryRedisBytes.Run(cacheCtx, s.rdb, streamRecoveryRedisKeys).Int64()
		if err != nil {
			return 0, streamRecoveryContextError("history accounting unavailable")
		}
		return n, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked()
	return s.retained, nil
}
func (s *OpenAIGatewayService) StreamRecoveryContextRetainedBytes(ctx context.Context) (int64, error) {
	return s.getStreamRecoveryContextStore().RetainedBytes(ctx)
}

// One Redis hash-tag keeps every Lua key in a single cluster slot. Every script
// purges expired payload AND aliases. The namespace also has a physical TTL when
// idle; entry expiry never slides on lookup. Accounting/eviction/put are atomic.
var streamRecoveryRedisKeys = []string{
	"{s2a-stream-recovery}:data", "{s2a-stream-recovery}:aliases", "{s2a-stream-recovery}:sizes", "{s2a-stream-recovery}:upstream", "{s2a-stream-recovery}:expiry", "{s2a-stream-recovery}:lru", "{s2a-stream-recovery}:meta",
}

const streamRecoveryRedisPrelude = `
local t = redis.call('TIME')
local now = tonumber(t[1])*1000 + math.floor(tonumber(t[2])/1000)
local function remove(key)
  local size = tonumber(redis.call('HGET', KEYS[3], key)) or 0
  local alias = redis.call('HGET', KEYS[4], key)
  redis.call('HDEL', KEYS[1], key)
  redis.call('HDEL', KEYS[2], key)
  if alias and redis.call('HGET', KEYS[2], alias) == key then redis.call('HDEL', KEYS[2], alias) end
  redis.call('HDEL', KEYS[3], key)
  redis.call('HDEL', KEYS[4], key)
  redis.call('ZREM', KEYS[5], key)
  redis.call('ZREM', KEYS[6], key)
  local used = tonumber(redis.call('HGET', KEYS[7], 'bytes')) or 0
  redis.call('HSET', KEYS[7], 'bytes', math.max(0,used-size))
end
for _, key in ipairs(redis.call('ZRANGEBYSCORE', KEYS[5], '-inf', now)) do remove(key) end
`

var streamRecoveryRedisPut = redis.NewScript(streamRecoveryRedisPrelude + `
local canonical, alias = ARGV[1], ARGV[2]
local size, ttl, budget, cap = tonumber(ARGV[4]), tonumber(ARGV[5]), tonumber(ARGV[6]), tonumber(ARGV[7])
if size > budget or size > 8388608 then return 0 end
local collision = redis.call('HGET', KEYS[2], alias)
if collision and collision ~= canonical then return 0 end
local canonicalCollision = redis.call('HGET', KEYS[2], canonical)
if canonicalCollision and canonicalCollision ~= canonical then return 0 end
remove(canonical)
local used = tonumber(redis.call('HGET', KEYS[7], 'bytes')) or 0
while used+size > budget or redis.call('HLEN', KEYS[1]) >= cap do
  local victim = redis.call('ZRANGE', KEYS[6], 0, 0)
  if #victim == 0 then return 0 end
  remove(victim[1])
  used = tonumber(redis.call('HGET', KEYS[7], 'bytes')) or 0
end
redis.call('HSET', KEYS[1], canonical, ARGV[3])
redis.call('HSET', KEYS[2], canonical, canonical, alias, canonical)
redis.call('HSET', KEYS[3], canonical, size)
redis.call('HSET', KEYS[4], canonical, alias)
redis.call('ZADD', KEYS[5], now+ttl, canonical)
redis.call('ZADD', KEYS[6], now, canonical)
redis.call('HSET', KEYS[7], 'bytes', used+size)
for _, key in ipairs(KEYS) do redis.call('PEXPIRE', key, ttl) end
return 1
`)
var streamRecoveryRedisLength = redis.NewScript(streamRecoveryRedisPrelude + `
local canonical = redis.call('HGET', KEYS[2], ARGV[1])
if not canonical then return 0 end
return redis.call('HSTRLEN', KEYS[1], canonical)
`)
var streamRecoveryRedisGet = redis.NewScript(streamRecoveryRedisPrelude + `
local canonical = redis.call('HGET', KEYS[2], ARGV[1])
if not canonical then return false end
local length = redis.call('HSTRLEN', KEYS[1], canonical)
local maxRead = tonumber(ARGV[2])
if not maxRead or maxRead < 1 or maxRead > 8388608 or length > maxRead or length > 8388608 then
  return redis.error_reply('stream recovery entry changed beyond charged read budget')
end
local payload = redis.call('HGET', KEYS[1], canonical)
if not payload then return false end
redis.call('ZADD', KEYS[6], now, canonical)
return payload
`)
var streamRecoveryRedisBytes = redis.NewScript(streamRecoveryRedisPrelude + `return tonumber(redis.call('HGET', KEYS[7], 'bytes')) or 0`)
