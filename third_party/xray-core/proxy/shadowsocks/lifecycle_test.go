package shadowsocks

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/protocol"
)

func lifecycleTestUser(t *testing.T, email string) *protocol.MemoryUser {
	t.Helper()
	account, err := (&Account{
		Password:   "lifecycle-test-password",
		CipherType: CipherType_AES_128_GCM,
	}).AsAccount()
	if err != nil {
		t.Fatal(err)
	}
	return &protocol.MemoryUser{Email: email, Account: account}
}

func TestAttackDefenseBoundsRecordsAtInsertion(t *testing.T) {
	defense := NewAttackDefense(&DefenseConfig{
		MaxFailures:      20,
		BanDuration:      time.Hour,
		CleanupInterval:  time.Hour,
		EarlyStopPercent: 30,
	})
	defer defense.Close()

	insertions := attackDefenseShardCount * (maxDefenseRecordsPerShard + 256)
	lastAddress := ""
	for i := 0; i < insertions; i++ {
		lastAddress = fmt.Sprintf("source-%d:443", i)
		defense.RecordFailure(lastAddress)
	}

	total := 0
	for i, shard := range defense.shards {
		shard.mu.RLock()
		count := len(shard.records)
		shard.mu.RUnlock()
		if count > maxDefenseRecordsPerShard {
			t.Fatalf("shard %d contains %d records, limit is %d", i, count, maxDefenseRecordsPerShard)
		}
		total += count
	}
	if total > attackDefenseShardCount*maxDefenseRecordsPerShard {
		t.Fatalf("defense contains %d records above the hard limit", total)
	}
	if !defense.HasFailureRecord(lastAddress) {
		t.Fatal("most recently inserted failure record was evicted")
	}
}

func TestAttackDefenseBoundsWhitelistAtInsertion(t *testing.T) {
	defense := NewAttackDefense(&DefenseConfig{
		MaxFailures:     20,
		BanDuration:     time.Hour,
		CleanupInterval: time.Hour,
	})
	defer defense.Close()

	insertions := attackDefenseShardCount * (maxWhitelistPerShard + 256)
	for i := 0; i < insertions; i++ {
		defense.RecordSuccess(fmt.Sprintf("valid-source-%d:443", i))
	}
	total := 0
	for i, shard := range defense.whitelist {
		shard.mu.RLock()
		count := len(shard.entries)
		shard.mu.RUnlock()
		if count > maxWhitelistPerShard {
			t.Fatalf("whitelist shard %d contains %d entries, limit is %d", i, count, maxWhitelistPerShard)
		}
		total += count
	}
	if total > attackDefenseShardCount*maxWhitelistPerShard {
		t.Fatalf("whitelist contains %d entries above the hard limit", total)
	}
}

func TestAttackDefenseCloseStopsCleanupAndReleasesRecords(t *testing.T) {
	defense := NewAttackDefense(&DefenseConfig{
		MaxFailures:     20,
		BanDuration:     time.Hour,
		CleanupInterval: time.Hour,
	})
	doneCh := defense.doneCh
	defense.RecordFailure("192.0.2.1:443")

	if err := defense.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-doneCh:
	default:
		t.Fatal("cleanup goroutine is still running after Close")
	}

	for i, shard := range defense.shards {
		shard.mu.RLock()
		records := shard.records
		order := shard.order
		shard.mu.RUnlock()
		if records != nil || order != nil {
			t.Fatalf("shard %d retained storage after Close", i)
		}
		whitelist := defense.whitelist[i]
		whitelist.mu.RLock()
		entries := whitelist.entries
		whitelistOrder := whitelist.order
		whitelist.mu.RUnlock()
		if entries != nil || whitelistOrder != nil {
			t.Fatalf("whitelist shard %d retained storage after Close", i)
		}
	}

	defense.RecordFailure("192.0.2.2:443")
	if stats := defense.GetStats(); stats.TotalRecords != 0 {
		t.Fatalf("closed defense accepted %d new records", stats.TotalRecords)
	}
	if err := defense.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAttackDefenseConcurrentClose(t *testing.T) {
	defense := NewAttackDefense(&DefenseConfig{
		MaxFailures:     20,
		BanDuration:     time.Hour,
		CleanupInterval: time.Hour,
	})

	start := make(chan struct{})
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		worker := worker
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for i := 0; i < 4000; i++ {
				address := fmt.Sprintf("worker-%d-source-%d:443", worker, i)
				defense.RecordFailure(address)
				_ = defense.CheckAllowed(address)
			}
		}()
	}
	close(start)
	if err := defense.Close(); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
}

func TestServerCloseReleasesShadowsocksState(t *testing.T) {
	validator := new(Validator)
	user := lifecycleTestUser(t, "lifecycle@example.com")
	if err := validator.Add(user); err != nil {
		t.Fatal(err)
	}
	if validator.GetByEmail(user.Email) != user {
		t.Fatal("validator failed to return the configured user before Close")
	}

	cache := validator.userCache
	defense := validator.attackDefense
	cache.PutWithSuccess("192.0.2.10:12345", user)
	defense.RecordFailure("192.0.2.11:443")

	server := &Server{validator: validator}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}

	validator.RLock()
	closed := validator.closed
	userCount := len(validator.users)
	validator.RUnlock()
	if !closed || userCount != 0 {
		t.Fatalf("validator close state: closed=%v users=%d", closed, userCount)
	}
	if validator.GetByEmail(user.Email) != nil {
		t.Fatal("closed validator retained its user index")
	}
	if err := validator.Add(user); err == nil {
		t.Fatal("closed validator accepted a new user")
	}

	for i, shard := range cache.ipShards {
		shard.mu.RLock()
		entries := shard.cache
		list := shard.list
		shard.mu.RUnlock()
		if entries != nil || list != nil {
			t.Fatalf("cache shard %d retained storage after Server.Close", i)
		}
	}
	successUsers := 0
	cache.GetSuccessUserMap().Range(func(_, _ interface{}) bool {
		successUsers++
		return true
	})
	if successUsers != 0 {
		t.Fatalf("success-user cache retained %d users after Server.Close", successUsers)
	}
	select {
	case <-defense.doneCh:
	default:
		t.Fatal("Server.Close did not stop attack-defense cleanup")
	}
}
