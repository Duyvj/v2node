package shadowsocks

import (
	"crypto/cipher"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
)

type blockingOpenAEAD struct {
	cipher.AEAD
	entered chan struct{}
	release <-chan struct{}
	once    *sync.Once
}

func (a *blockingOpenAEAD) Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error) {
	a.once.Do(func() {
		close(a.entered)
		<-a.release
	})
	return a.AEAD.Open(dst, nonce, ciphertext, additionalData)
}

func cacheTestUser(t *testing.T, email, password string) *protocol.MemoryUser {
	t.Helper()
	account, err := (&Account{
		Password:   password,
		CipherType: CipherType_AES_128_GCM,
	}).AsAccount()
	if err != nil {
		t.Fatal(err)
	}
	return &protocol.MemoryUser{Email: email, Account: account}
}

func cacheTestPacket(t *testing.T, user *protocol.MemoryUser) []byte {
	t.Helper()
	request := &protocol.RequestHeader{
		Version: Version,
		User:    user,
		Command: protocol.RequestCommandUDP,
		Address: xnet.LocalHostIP,
		Port:    1234,
	}
	packet, err := EncodeUDPPacket(request, []byte("cache-revocation-test"))
	if err != nil {
		t.Fatal(err)
	}
	defer packet.Release()
	return append([]byte(nil), packet.Bytes()...)
}

func blockNextAuthentication(t *testing.T, user *protocol.MemoryUser) (<-chan struct{}, chan<- struct{}) {
	t.Helper()
	aeadCipher := user.Account.(*MemoryAccount).Cipher.(*AEADCipher)
	originalCreator := aeadCipher.AEADAuthCreator
	entered := make(chan struct{})
	release := make(chan struct{})
	once := new(sync.Once)
	aeadCipher.AEADAuthCreator = func(key []byte) cipher.AEAD {
		return &blockingOpenAEAD{
			AEAD:    originalCreator(key),
			entered: entered,
			release: release,
			once:    once,
		}
	}
	return entered, release
}

func waitForAuthentication(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("authentication did not reach the controlled interleaving point")
	}
}

func assertUserAbsentFromCache(t *testing.T, cache *UserCache, cacheKey string, user *protocol.MemoryUser) {
	t.Helper()
	for _, cached := range cache.GetMultiUser(cacheKey) {
		if cached == user {
			t.Fatal("revoked user remained in the first-level cache")
		}
	}
	if value, ok := cache.successCache.users.Load(normalizeUserEmail(user.Email)); ok {
		if entry, entryOK := value.(*successUserEntry); entryOK && entry.user == user {
			t.Fatal("revoked user remained in the second-level cache")
		}
	}
}

type authenticationResult struct {
	user *protocol.MemoryUser
	err  error
}

func successCacheEntryCount(cache *UserCache) int {
	count := 0
	cache.GetSuccessUserMap().Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
}

func sameShardKeys(cache *UserCache, count int) []string {
	keys := make([]string, 0, count)
	var target *userCacheShard
	for i := 0; len(keys) < count; i++ {
		key := fmt.Sprintf("192.0.2.%d:%d", i%255, 10000+i)
		shard := cache.getShard(key)
		if target == nil {
			target = shard
		}
		if shard == target {
			keys = append(keys, key)
		}
	}
	return keys
}

func assertShardConsistent(t *testing.T, shard *userCacheShard) {
	t.Helper()
	shard.mu.RLock()
	defer shard.mu.RUnlock()
	if shard.list == nil {
		t.Fatal("cache shard list is nil")
	}
	seen := make(map[string]bool, shard.list.size)
	count := 0
	for node := shard.list.head.next; node != shard.list.tail; node = node.next {
		if node == nil {
			t.Fatal("LRU list terminated before its tail sentinel")
		}
		if seen[node.key] {
			t.Fatalf("duplicate LRU node for %q", node.key)
		}
		seen[node.key] = true
		entry, ok := shard.cache[node.key]
		if !ok || entry.node != node {
			t.Fatalf("LRU node %q has no matching map entry", node.key)
		}
		count++
	}
	if count != shard.list.size || count != len(shard.cache) {
		t.Fatalf("cache/list size mismatch: list traversal=%d list.size=%d map=%d", count, shard.list.size, len(shard.cache))
	}
	if shard.list.size > shard.cap {
		t.Fatalf("cache list exceeds capacity: size=%d cap=%d", shard.list.size, shard.cap)
	}
}

func TestUserCacheDelayedLRURefreshIgnoresDeletedAndEvictedEntries(t *testing.T) {
	for _, tc := range []struct {
		name           string
		mutate         func(*userCacheShard, string, string)
		wantKeyPresent bool
	}{
		{
			name: "delete",
			mutate: func(shard *userCacheShard, key, _ string) {
				shard.mu.Lock()
				entry := shard.cache[key]
				shard.list.remove(entry.node)
				delete(shard.cache, key)
				shard.mu.Unlock()
			},
		},
		{
			name: "eviction",
			mutate: func(shard *userCacheShard, key, replacement string) {
				shard.mu.Lock()
				shard.cap = 1
				shard.mu.Unlock()
				shard.putMultiUser(replacement, &protocol.MemoryUser{Email: "replacement@example.com"})
				shard.putMultiUser(key, &protocol.MemoryUser{Email: "new@example.com"})
			},
			wantKeyPresent: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewUserCache(256)
			defer cache.Close()
			keys := sameShardKeys(cache, 2)
			key, replacement := keys[0], keys[1]
			shard := cache.getShard(key)
			shard.putMultiUser(key, &protocol.MemoryUser{Email: "stale@example.com"})
			shard.mu.Lock()
			shard.cache[key].lastAccess = time.Now().Add(-6 * time.Second).UnixNano()
			shard.mu.Unlock()

			// This is the lookup portion of get/getMultiUser. The mutation is
			// deliberately performed before the delayed refresh phase.
			shard.mu.RLock()
			entry := shard.cache[key]
			shard.mu.RUnlock()
			if entry == nil {
				t.Fatal("cache lookup did not find test entry")
			}
			tc.mutate(shard, key, replacement)

			shard.refreshLRU(key, entry, time.Now().UnixNano())
			assertShardConsistent(t, shard)
			shard.mu.RLock()
			current, stillPresent := shard.cache[key]
			shard.mu.RUnlock()
			if stillPresent != tc.wantKeyPresent {
				t.Fatalf("%s mutation key presence=%v, want %v", tc.name, stillPresent, tc.wantKeyPresent)
			}
			if current == entry {
				t.Fatalf("%s mutation left stale entry mapped", tc.name)
			}
		})
	}
}

func TestValidatorDeleteClearsMixedCaseUserFromCaches(t *testing.T) {
	validator := new(Validator)
	defer validator.Close()

	user := lifecycleTestUser(t, "Mixed.Case@Example.COM")
	if err := validator.Add(user); err != nil {
		t.Fatal(err)
	}
	cacheKey := "192.0.2.10:443"
	validator.userCache.PutWithSuccess(cacheKey, user)

	normalizedEmail := "mixed.case@example.com"
	if _, ok := validator.userCache.successCache.users.Load(normalizedEmail); !ok {
		t.Fatal("successful user was not stored under its normalized email")
	}
	if _, ok := validator.userCache.successCache.users.Load(user.Email); ok {
		t.Fatal("successful user was also stored under its original-case email")
	}

	if err := validator.Del(normalizedEmail); err != nil {
		t.Fatal(err)
	}
	if got := validator.GetByEmail(user.Email); got != nil {
		t.Fatal("deleted mixed-case user remained in the validator index")
	}
	if users := validator.userCache.GetMultiUser(cacheKey); len(users) != 0 {
		t.Fatalf("deleted mixed-case user remained in the IP cache: %v", users)
	}
	if count := successCacheEntryCount(validator.userCache); count != 0 {
		t.Fatalf("deleted mixed-case user left %d successful-user cache entries", count)
	}
}

func TestUserCacheRemoveClearsEveryEntryInShard(t *testing.T) {
	cache := NewUserCache(256)
	defer cache.Close()

	target := &protocol.MemoryUser{Email: "repeated.user@example.com"}
	other := &protocol.MemoryUser{Email: "other@example.com"}
	keys := sameShardKeys(cache, 4)
	for _, key := range keys {
		cache.PutMultiUser(key, target)
	}
	cache.PutMultiUser(keys[0], other)

	cache.Remove(target.Email)

	for _, key := range keys {
		for _, user := range cache.GetMultiUser(key) {
			if normalizeUserEmail(user.Email) == normalizeUserEmail(target.Email) {
				t.Fatalf("deleted user remained cached under %q", key)
			}
		}
	}
	users := cache.GetMultiUser(keys[0])
	if len(users) != 1 || users[0] != other {
		t.Fatalf("removal damaged the non-target user: %v", users)
	}
}

func TestSuccessUserCacheEnforcesCapacity(t *testing.T) {
	cache := NewUserCache(256)
	defer cache.Close()
	cache.successCache.cap = 3
	cache.successCache.ttl = 24 * time.Hour

	for _, email := range []string{"old@example.com", "two@example.com", "three@example.com"} {
		cache.addSuccessUser(&protocol.MemoryUser{Email: email})
	}
	value, ok := cache.successCache.users.Load("old@example.com")
	if !ok {
		t.Fatal("oldest cache entry was not inserted")
	}
	atomic.StoreInt64(&value.(*successUserEntry).lastAccess, time.Now().Add(-time.Hour).UnixNano())

	cache.addSuccessUser(&protocol.MemoryUser{Email: "four@example.com"})
	if _, ok := cache.successCache.users.Load("old@example.com"); ok {
		t.Fatal("capacity eviction did not remove the least-recently used entry")
	}
	if count := successCacheEntryCount(cache); count != cache.successCache.cap {
		t.Fatalf("successful-user cache contains %d entries, want %d", count, cache.successCache.cap)
	}

	var workers sync.WaitGroup
	for i := 0; i < 64; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			cache.addSuccessUser(&protocol.MemoryUser{Email: fmt.Sprintf("concurrent-%d@example.com", i)})
		}(i)
	}
	workers.Wait()
	if count := successCacheEntryCount(cache); count > cache.successCache.cap {
		t.Fatalf("concurrent inserts grew successful-user cache to %d entries, limit is %d", count, cache.successCache.cap)
	}
}

func TestSuccessUserCacheExpiresIdleEntries(t *testing.T) {
	cache := NewUserCache(256)
	defer cache.Close()
	cache.successCache.cap = 8
	cache.successCache.ttl = time.Minute

	cache.addSuccessUser(&protocol.MemoryUser{Email: "expired@example.com"})
	cache.addSuccessUser(&protocol.MemoryUser{Email: "fresh@example.com"})
	value, ok := cache.successCache.users.Load("expired@example.com")
	if !ok {
		t.Fatal("expiring cache entry was not inserted")
	}
	atomic.StoreInt64(&value.(*successUserEntry).lastAccess, time.Now().Add(-cache.successCache.ttl).UnixNano())

	successUsers := cache.GetSuccessUserMap()
	if _, ok := successUsers.Load("expired@example.com"); ok {
		t.Fatal("idle successful-user cache entry did not expire")
	}
	if _, ok := successUsers.Load("fresh@example.com"); !ok {
		t.Fatal("expiry removed a fresh successful-user cache entry")
	}
}

func TestExpiredSuccessCacheEntryFallsBackToFullAuthentication(t *testing.T) {
	validator := new(Validator)
	defer validator.Close()
	user := lifecycleTestUser(t, "fallback@example.com")
	if err := validator.Add(user); err != nil {
		t.Fatal(err)
	}
	validator.userCache.successCache.ttl = time.Minute

	request := &protocol.RequestHeader{
		Version: Version,
		User:    user,
		Command: protocol.RequestCommandUDP,
		Address: xnet.LocalHostIP,
		Port:    1234,
	}
	firstPacket, err := EncodeUDPPacket(request, []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeUDPPacketWithCache(validator, firstPacket, "192.0.2.1:1000"); err != nil {
		firstPacket.Release()
		t.Fatal(err)
	}
	firstPacket.Release()

	value, ok := validator.userCache.successCache.users.Load(normalizeUserEmail(user.Email))
	if !ok {
		t.Fatal("successful authentication did not populate the second-level cache")
	}
	atomic.StoreInt64(&value.(*successUserEntry).lastAccess, time.Now().Add(-validator.userCache.successCache.ttl).UnixNano())

	secondPacket, err := EncodeUDPPacket(request, []byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	defer secondPacket.Release()
	decoded, _, err := DecodeUDPPacketWithCache(validator, secondPacket, "192.0.2.2:2000")
	if err != nil {
		t.Fatalf("authentication failed after successful-user cache expiry: %v", err)
	}
	if decoded.User != user {
		t.Fatal("full authentication fallback returned the wrong user")
	}
}

func TestGetWithCacheDeleteCannotResurrectSecondLevelHit(t *testing.T) {
	validator := new(Validator)
	defer validator.Close()

	user := cacheTestUser(t, "delete-race@example.com", "old-password")
	if err := validator.Add(user); err != nil {
		t.Fatal(err)
	}
	packet := cacheTestPacket(t, user)
	validator.userCache.PutWithSuccess("192.0.2.20:2000", user)

	entered, release := blockNextAuthentication(t, user)
	cacheKey := "192.0.2.21:2001" // First-level miss forces the second-level path.
	result := make(chan authenticationResult, 1)
	go func() {
		authenticated, _, _, _, err := validator.GetWithCache(packet, protocol.RequestCommandUDP, cacheKey)
		result <- authenticationResult{user: authenticated, err: err}
	}()

	waitForAuthentication(t, entered)
	if err := validator.Del(user.Email); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)

	select {
	case got := <-result:
		if got.err != ErrNotFound || got.user != nil {
			t.Fatalf("deleted cached credential authenticated: user=%p err=%v", got.user, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("authentication did not finish after deletion")
	}
	assertUserAbsentFromCache(t, validator.userCache, cacheKey, user)
}

func TestGetWithCachePasswordRotationCannotResurrectFirstLevelHit(t *testing.T) {
	validator := new(Validator)
	defer validator.Close()

	oldUser := cacheTestUser(t, "rotation-race@example.com", "old-password")
	newUser := cacheTestUser(t, oldUser.Email, "new-password")
	if err := validator.Add(oldUser); err != nil {
		t.Fatal(err)
	}
	oldPacket := cacheTestPacket(t, oldUser)
	cacheKey := "192.0.2.30:3000"
	validator.userCache.PutWithSuccess(cacheKey, oldUser)

	entered, release := blockNextAuthentication(t, oldUser)
	result := make(chan authenticationResult, 1)
	go func() {
		authenticated, _, _, _, err := validator.GetWithCache(oldPacket, protocol.RequestCommandUDP, cacheKey)
		result <- authenticationResult{user: authenticated, err: err}
	}()

	waitForAuthentication(t, entered)
	if err := validator.UpdateUser(oldUser.Email, newUser); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)

	select {
	case got := <-result:
		if got.err != ErrNotFound || got.user != nil {
			t.Fatalf("rotated cached credential authenticated: user=%p err=%v", got.user, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("authentication did not finish after password rotation")
	}
	assertUserAbsentFromCache(t, validator.userCache, cacheKey, oldUser)

	newPacket := cacheTestPacket(t, newUser)
	authenticated, _, _, _, err := validator.GetWithCache(newPacket, protocol.RequestCommandUDP, cacheKey)
	if err != nil || authenticated != newUser {
		t.Fatalf("rotated credential did not authenticate: user=%p err=%v", authenticated, err)
	}
	users := validator.userCache.GetMultiUser(cacheKey)
	if len(users) != 1 || users[0] != newUser {
		t.Fatalf("first-level cache was not refreshed with the rotated user: %v", users)
	}
}

func TestGetWithCacheSequentialRevocation(t *testing.T) {
	t.Run("delete", func(t *testing.T) {
		validator := new(Validator)
		defer validator.Close()
		user := cacheTestUser(t, "sequential-delete@example.com", "password")
		if err := validator.Add(user); err != nil {
			t.Fatal(err)
		}
		packet := cacheTestPacket(t, user)
		cacheKey := "192.0.2.40:4000"
		validator.userCache.PutWithSuccess(cacheKey, user)
		if err := validator.Del(user.Email); err != nil {
			t.Fatal(err)
		}

		authenticated, _, _, _, err := validator.GetWithCache(packet, protocol.RequestCommandUDP, cacheKey)
		if err != ErrNotFound || authenticated != nil {
			t.Fatalf("sequentially deleted credential authenticated: user=%p err=%v", authenticated, err)
		}
		assertUserAbsentFromCache(t, validator.userCache, cacheKey, user)
	})

	t.Run("password rotation", func(t *testing.T) {
		validator := new(Validator)
		defer validator.Close()
		oldUser := cacheTestUser(t, "sequential-rotation@example.com", "old-password")
		newUser := cacheTestUser(t, oldUser.Email, "new-password")
		if err := validator.Add(oldUser); err != nil {
			t.Fatal(err)
		}
		oldPacket := cacheTestPacket(t, oldUser)
		newPacket := cacheTestPacket(t, newUser)
		cacheKey := "192.0.2.41:4001"
		validator.userCache.PutWithSuccess(cacheKey, oldUser)
		if err := validator.UpdateUser(oldUser.Email, newUser); err != nil {
			t.Fatal(err)
		}

		authenticated, _, _, _, err := validator.GetWithCache(oldPacket, protocol.RequestCommandUDP, cacheKey)
		if err != ErrNotFound || authenticated != nil {
			t.Fatalf("old sequential credential authenticated: user=%p err=%v", authenticated, err)
		}
		authenticated, _, _, _, err = validator.GetWithCache(newPacket, protocol.RequestCommandUDP, cacheKey)
		if err != nil || authenticated != newUser {
			t.Fatalf("new sequential credential failed: user=%p err=%v", authenticated, err)
		}
	})
}
