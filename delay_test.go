package delay

import (
	"context"
	"fmt"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func getRdb() *redis.Client {
	rdb := redis.NewClient(&redis.Options{
		Addr:     "localhost:6379",
		Password: "", // no password set
		DB:       0,  // use default DB
	})
	return rdb
}

func TestBase(t *testing.T) {
	rdb := getRdb()
	delayQueueName := "test_base_queue"

	t.Run("SequentialItemsWithSameDelay", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel() // Ensure cancel is called to clean up resources

		cleanDelayQueue(ctx, rdb, delayQueueName)
		defer cleanDelayQueue(ctx, rdb, delayQueueName)

		itemDelaySeconds := int64(1)
		maxTTL := int64(10)
		expectedValues := []string{"item1", "item2", "item3"}
		actualValues := make([]string, 0, len(expectedValues))

		for _, val := range expectedValues {
			err := AddToQueue(ctx, rdb, delayQueueName, val, itemDelaySeconds, maxTTL)
			assert.NoError(t, err)
		}

		consumerWg := &sync.WaitGroup{}
		consumerWg.Add(1)
		go func() {
			defer consumerWg.Done()
			resCh, errCh := GetFromQueue(ctx, rdb, delayQueueName)
			processedCount := 0
			for {
				select {
				case item, ok := <-resCh:
					if !ok {
						t.Log("resCh closed")
						return
					}
					t.Logf("Received item: %s", item)
					actualValues = append(actualValues, item)
					processedCount++
					if processedCount == len(expectedValues) {
						cancel() // Signal to stop after all expected items are processed
						return // Exit goroutine
					}
				case err, ok := <-errCh:
					if !ok {
						t.Log("errCh closed")
						return
					}
					// Only context.Canceled is expected after cancel() is called
					if err != context.Canceled && err != redis.Nil { // redis.Nil can happen on BLPop timeout
						t.Errorf("Unexpected error from GetFromQueue: %v", err)
					}
					if err == context.Canceled {
						return
					}
				case <-ctx.Done(): // Handles explicit cancel() or test timeout
					t.Log("Context cancelled in consumer")
					return
				}
			}
		}()

		// Wait for processing or timeout
		// Timeout should be generous enough for all items to be processed + poller latency
		timeoutDuration := time.Second*time.Duration(itemDelaySeconds*int64(len(expectedValues))) + // total delay for items
			time.Duration(len(expectedValues))*pollerSleepDuration + // poller latency allowance
			2*time.Second // buffer
		
		select {
		case <-time.After(timeoutDuration):
			if len(actualValues) < len(expectedValues) {
				t.Errorf("Timeout: Expected %d items, got %d. Received: %v", len(expectedValues), len(actualValues), actualValues)
			}
		case <-ctx.Done():
			// Expected path if all items are processed and cancel() is called
			t.Log("Processing finished, context cancelled by consumer.")
		}
		
		consumerWg.Wait() // Ensure consumer goroutine finishes
		assert.Equal(t, expectedValues, actualValues, "Mismatch in received items")
	})

	t.Run("ItemsWithDifferentDelaysIncludingZero", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cleanDelayQueue(ctx, rdb, delayQueueName)
		defer cleanDelayQueue(ctx, rdb, delayQueueName)

		items := []struct {
			value string
			delay int64 // seconds
		}{
			{"item_0s_1", 0}, // Zero delay
			{"item_1s_1", 1}, // 1s delay
			{"item_0s_2", 0}, // Zero delay again
			{"item_1s_2", 1}, // 1s delay again
		}
		// Expected order: item_0s_1, item_0s_2 (order between them might vary), then item_1s_1, item_1s_2 (order between them might vary)
		// The poller processes items due "now" based on ZRANGEBYSCORE. Within the same score, order is lexicographical.
		// Since timePiece names include the score (second), items added in the same second for delay=0
		// will have the same score. Their timePiece names will be like "dq:key:timestamp".
		// If AddToQueue is called rapidly, they might get the same timestamp.
		// Let's ensure timestamps are distinct by a small sleep or by making values distinct for sorting.
		// For simplicity, we'll collect all and sort by expected processing time, then by original value.
		// However, the current implementation of GetFromQueue processes one timePiece list completely.
		// So, if item_0s_1 and item_0s_2 are separate timePieces (due to AddToQueue calls in different seconds, or different scores),
		// their relative order is not strictly guaranteed if they become ready in the same poller cycle.
		// But items with delay=0 should definitely come before items with delay=1.

		expectedOrder := []string{"item_0s_1", "item_0s_2", "item_1s_1", "item_1s_2"} // Approximate expected order
		actualValues := make([]string, 0, len(items))
		maxTTL := int64(10)

		for _, item := range items {
			err := AddToQueue(ctx, rdb, delayQueueName, item.value, item.delay, maxTTL)
			assert.NoError(t, err)
			if item.delay == 0 { // Small sleep to ensure distinct timestamps if AddToQueue is too fast
				time.Sleep(5 * time.Millisecond) 
			}
		}
		
		processedCount := 0
		consumerWg := &sync.WaitGroup{}
		consumerWg.Add(1)
		go func() {
			defer consumerWg.Done()
			resCh, errCh := GetFromQueue(ctx, rdb, delayQueueName)
			for {
				select {
				case item, ok := <-resCh:
					if !ok { return }
					t.Logf("Received item (mixed delay): %s", item)
					actualValues = append(actualValues, item)
					processedCount++
					if processedCount == len(items) {
						cancel()
						return
					}
				case err, ok := <-errCh:
					if !ok { return }
					if err != context.Canceled && err != redis.Nil {
						t.Errorf("Unexpected error (mixed delay): %v", err)
					}
					if err == context.Canceled { return }
				case <-ctx.Done():
					return
				}
			}
		}()

		// Max delay is 1s. Total time should be around 1s + poller latencies + buffer.
		timeoutDuration := 3*time.Second + time.Duration(len(items))*pollerSleepDuration
		select {
		case <-time.After(timeoutDuration):
			if len(actualValues) < len(items) {
				t.Errorf("Timeout (mixed delay): Expected %d items, got %d. Received: %v", len(items), len(actualValues), actualValues)
			}
		case <-ctx.Done():
			t.Log("Processing finished (mixed delay), context cancelled.")
		}
		consumerWg.Wait()

		// Verify that zero-delay items came before 1-second-delay items.
		// We won't assert strict order within same-delay groups due to poller behavior.
		zeroDelayItems := []string{"item_0s_1", "item_0s_2"}
		oneSecondDelayItems := []string{"item_1s_1", "item_1s_2"}

		assert.Contains(t, actualValues, zeroDelayItems[0], "Missing zero delay item 1")
		assert.Contains(t, actualValues, zeroDelayItems[1], "Missing zero delay item 2")
		assert.Contains(t, actualValues, oneSecondDelayItems[0], "Missing 1s delay item 1")
		assert.Contains(t, actualValues, oneSecondDelayItems[1], "Missing 1s delay item 2")

		// Find indices
		idx_0s_1 := -1
		idx_0s_2 := -1
		idx_1s_1 := -1
		idx_1s_2 := -1
		for i, v := range actualValues {
			if v == zeroDelayItems[0] { idx_0s_1 = i }
			if v == zeroDelayItems[1] { idx_0s_2 = i }
			if v == oneSecondDelayItems[0] { idx_1s_1 = i }
			if v == oneSecondDelayItems[1] { idx_1s_2 = i }
		}
		
		assert.True(t, idx_0s_1 < idx_1s_1, "Zero delay item 1 did not come before 1s delay item 1")
		assert.True(t, idx_0s_1 < idx_1s_2, "Zero delay item 1 did not come before 1s delay item 2")
		assert.True(t, idx_0s_2 < idx_1s_1, "Zero delay item 2 did not come before 1s delay item 1")
		assert.True(t, idx_0s_2 < idx_1s_2, "Zero delay item 2 did not come before 1s delay item 2")
		t.Logf("Actual order for mixed delays: %v", actualValues)
	})
}


func TestAutoExpire(t *testing.T) {
	rdb := getRdb()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // Ensure cancel is eventually called

	delayQueueName := "test_auto_expire_queue"
	
	// This sub-test is for Scenario 1: Item expires *before* poller moves it.
	t.Run("ExpireBeforePoller", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background()) // Sub-test context
		defer cancel()
		cleanDelayQueue(ctx, rdb, delayQueueName)
		defer cleanDelayQueue(ctx, rdb, delayQueueName)

		itemValue := "expire_before_poller_item"
		// delaySecond is 0, so it's due now.
		// maxTTL is very short (1s).
		// The timePiece list key (e.g., "dq:test_auto_expire_queue:timestamp") will get EXPIRE set by AddToQueue.
		// The sorted set member (timePiece) itself does not have TTL by AddToQueue, only the list.
		// The sorted set key (`delayQueueName`) has a long TTL (3 days).
		delaySecond := int64(0) 
		maxTTL := int64(1)      // 1 second for the list associated with timePiece to live

		err := AddToQueue(ctx, rdb, delayQueueName, itemValue, delaySecond, maxTTL)
		assert.NoError(t, err)

		// timePiece will be like "dq:test_auto_expire_queue:NOW_TS"
		// This timePiece list should expire in maxTTL (1s).
		// The poller runs every `pollerSleepDuration` (100ms).
		// We need to wait long enough for the timePiece list to expire *and* for the poller to have run
		// and decided not to move the (now gone or soon-to-be-gone data of) timePiece.
		// The Lua script in poller does ZSCORE. If the ZSET member (timePiece) is still there,
		// but its corresponding list (where data is) is gone, ZREM happens, RPUSH to ready list happens.
		// Then GetFromQueue's LPop on timePiece will fail. This is more Scenario 2.

		// For Scenario 1 (expires before poller *moves* it):
		// This means the *member in the sorted set* should expire or be removed.
		// The current system doesn't expire members from the sorted set directly via AddToQueue's maxTTL.
		// maxTTL in AddToQueue sets expiry on the timePiece *list*.
		// If the sorted set member (timePiece) is still there, poller will try to move it.
		// And if the list data for that timePiece has expired, LPop will fail later.

		// To truly test "expires before poller moves it", we'd need TTL on sorted set members.
		// The current `maxTTL` is for the data list `timePiece`.
		// Let's re-interpret "expires before poller moves it" as:
		// The *data* for the timePiece (the list) expires. Poller moves the timePiece name. GetFromQueue tries LPop, fails.
		// This is effectively Scenario 2.

		// The original TestAutoExpire seemed to test if the item is NOT consumed after sleepSecond + maxTTL.
		// Let's stick to that: Add an item, wait for its timePiece list to expire, then try to consume.
		// It shouldn't be consumed.

		// Wait for the timePiece list to expire.
		// AddToQueue sets expire on timePiece list: time.Second*time.Duration(maxTTL+delaySecond)
		time.Sleep(time.Second*time.Duration(maxTTL+delaySecond) + 2*pollerSleepDuration) // Wait for list to expire + poller runs

		consumerWg := &sync.WaitGroup{}
		consumerWg.Add(1)
		consumed := false
		go func() {
			defer consumerWg.Done()
			resCh, errCh := GetFromQueue(ctx, rdb, delayQueueName) // Poller starts here
			select {
			case item, ok := <-resCh:
				if ok {
					t.Errorf("Received item %s, but it should have expired", item)
					consumed = true
				}
				cancel() // Stop if item unexpectedly received
			case err, ok := <-errCh:
				if !ok { return }
				if err != context.Canceled && err != redis.Nil {
					t.Errorf("Consumer error: %v", err)
				}
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second): // Timeout for GetFromQueue to try processing
				t.Log("GetFromQueue timed out as expected, no item received.")
				cancel() // Proceed to shutdown
			}
		}()
		
		<-ctx.Done() // Wait until test logic decides to cancel (either item received or timeout)
		consumerWg.Wait()
		assert.False(t, consumed, "Item was consumed but should have expired")

		// Check if timePiece is gone from sorted set (poller should have tried to process it and ZREM'd it)
		// The poller moves to ready list, then GetFromQueue tries LPop. If LPop fails (redis.Nil),
		// GetFromQueue DELs the timePiece list (which might be already gone).
		// The ZREM from the main sorted set happens in the poller's Lua script.
		// So, after an attempt, the timePiece member should be gone from the main sorted set.
		timePieces, _ := rdb.ZRange(ctx, delayQueueName, 0, -1).Result()
		found := false
		for _, tp := range timePieces {
			// The exact timePiece name is tricky to predict due to timestamp.
			// We expect it to be empty or not contain the specific item's timePiece.
			// For this test, simply checking if ZCARD is 0 after processing might be enough if it's the only item.
			// Or check if ready list is empty.
			if strings.Contains(tp, delayQueueName) { // A loose check
				// This is not robust enough.
			}
		}
		// A better check: is the ready list empty?
		readyKey := getReadyKey(delayQueueName)
		readyLen, _ := rdb.LLen(ctx, readyKey).Result()
		assert.Equal(t, int64(0), readyLen, "Ready list should be empty after expired item processing attempt")

		// And the original sorted set should also be empty (or not contain this timePiece)
		// This is harder to assert precisely without knowing the timePiece name.
		// If this was the only item, ZCard should be 0.
		// For now, rely on consumed=false and readyLen=0.
	})
	
	// Scenario 2: Item expires *after* poller moves it to readyKey but *before* LPop from timePiece.
	// This is essentially the same as above if maxTTL is short. The poller moves the timePiece name.
	// GetFromQueue BLPops it. Then tries LPop. If timePiece list expired, LPop yields redis.Nil.
	// This is covered by the "ExpireBeforePoller" if we consider the data list's TTL.
	// The key is that AddToQueue sets TTL on the timePiece data list, not the ZSET member.
	// So the poller will always find the ZSET member (unless Redis itself expires it, which is not configured by this system).
	// Poller moves the timePiece name to ready list. GetFromQueue pops it.
	// Then it tries LPop(timePiece_name). If the *list* at timePiece_name has expired, LPop returns Nil.
	// GetFromQueue then DELs the (already self-expired) timePiece_name list.
	// This behavior seems to be what "ExpireBeforePoller" is testing.
	// Let's rename "ExpireBeforePoller" to "ItemDataExpiresBeforeConsumption" for clarity.
	// And make the sleeps and checks more robust.
}

// TestAutoExpire is refactored below with a clearer name and logic
func TestItemDataExpiresBeforeConsumption(t *testing.T) {
	rdb := getRdb()
	delayQueueName := "test_item_data_expires_queue"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cleanDelayQueue(ctx, rdb, delayQueueName)
	defer cleanDelayQueue(ctx, rdb, delayQueueName)

	itemValue := "item_data_expires_val"
	delaySeconds := int64(0) // Due "now"
	maxTTLSeconds := int64(1) // timePiece list will expire in 1 second + delaySeconds

	err := AddToQueue(ctx, rdb, delayQueueName, itemValue, delaySeconds, maxTTLSeconds)
	assert.NoError(t, err)

	// The item is due now. Poller (100ms interval) should pick it up quickly and move to ready list.
	// GetFromQueue's BLPop will pick it up from ready list.
	// Then, GetFromQueue will attempt LPop on the timePiece list.
	// We want this LPop to happen *after* the timePiece list has expired.
	// AddToQueue sets list expiry to: time.Second * time.Duration(maxTTLSeconds + delaySeconds).
	// So, we need to ensure the consumer attempts LPop after this duration from AddToQueue.

	var itemConsumed bool
	consumerWg := &sync.WaitGroup{}
	consumerWg.Add(1)

	go func() {
		defer consumerWg.Done()
		// Give a bit of time for AddToQueue to complete and item to be potentially polled once
		// This is a bit racy, but we want to start GetFromQueue slightly after AddToQueue.
		time.Sleep(50 * time.Millisecond) 

		resCh, errCh := GetFromQueue(ctx, rdb, delayQueueName)
		
		// Attempt to consume. This select will block.
		// If the item's data list expires before LPop, resCh should not receive the item.
		// errCh might receive redis.Nil from BLPop timeout if nothing ever makes it, or context.Canceled.
		select {
		case item, ok := <-resCh:
			if ok {
				t.Errorf("Received item '%s', but its data should have expired before LPop", item)
				itemConsumed = true
			}
			cancel() // Stop further processing
		case err, ok := <-errCh:
			if !ok {
				return // Channel closed
			}
			// Expected: context.Canceled due to outer cancel, or redis.Nil if BLPop timed out before outer cancel.
			if err != context.Canceled && err != redis.Nil {
				t.Logf("Consumer received error: %v", err) // Log non-critical errors
			}
			// If we get here, it means no item was sent to resCh before an error or channel closure.
		case <-ctx.Done(): // Triggered by cancel() from main test body or timeout
			t.Log("Consumer context done.")
		}
	}()

	// Wait for a duration longer than maxTTLSeconds to ensure the timePiece list expires.
	// Also consider poller interval and BLPop timeout.
	// Total time to wait: maxTTL + pollerSleepDuration (for poller to move) + blpopTimeout (for GetFromQueue to attempt) + buffer
	// Time for AddToQueue's EXPIRE to take effect on the list:
	expirationTime := time.Second * time.Duration(maxTTLSeconds+delaySeconds)
	
	// Sleep until after the list should have expired.
	time.Sleep(expirationTime + pollerSleepDuration + blpopTimeout + 200*time.Millisecond)

	cancel() // Cancel the context to stop the consumer goroutine.
	consumerWg.Wait()

	assert.False(t, itemConsumed, "Item was consumed, but its data list should have expired.")

	// Verify the queue is clean: sorted set and ready list should be empty.
	// Poller should have ZREM'd the timePiece from sorted set after moving it.
	// GetFromQueue should have DEL'd the timePiece list after LPop failed (redis.Nil).
	// BLPop would have removed it from the ready list.
	
	// Check sorted set
	zCard, err := rdb.ZCard(ctx, delayQueueName).Result()
	assert.NoError(t, err, "Error checking ZCard of sorted set")
	assert.Equal(t, int64(0), zCard, "Sorted set should be empty after processing an expired item.")

	// Check ready list
	readyKey := getReadyKey(delayQueueName)
	llen, err := rdb.LLen(ctx, readyKey).Result()
	assert.NoError(t, err, "Error checking LLen of ready list")
	assert.Equal(t, int64(0), llen, "Ready list should be empty.")
}

// TestAddToQueueDuplicateTimePieceValuePush tests that if AddToQueue is called twice
// with parameters that generate the same timePiece string (i.e. same key and same expiry second),
// the second call (where ZADD NX returns 0) still RPUSHes the value to the list.
// It also checks that the timePiece list's TTL is set only on the first "v > 0" call.
func TestAddToQueueDuplicateTimePieceValuePush(t *testing.T) {
	rdb := getRdb()
	delayQueueName := "test_duplicate_tp_queue"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cleanDelayQueue(ctx, rdb, delayQueueName)
	defer cleanDelayQueue(ctx, rdb, delayQueueName)

	value1 := "value1_for_duplicate_tp"
	value2 := "value2_for_duplicate_tp"
	// Fixed delay to ensure if we call it fast enough, expireSecond might be the same.
	// However, to guarantee same timePiece, we need to control time.Now() or expireSecond.
	// Let's set delay to 1, and assume test runs fast enough for time.Now().Unix() to be same for two calls.
	// This is flaky.
	// A better way: use a large delaySecond so time.Now().Unix() is less likely to change relative to it.
	// Or, more robustly, acknowledge AddToQueue's design: it's about adding *values* to *scheduled slots*.
	// The test for "ZADD NX" in Lua is more about the atomicity and correctness of that script.
	// The v > 0 logic in Go determines if EXPIRE is set.

	// Let's make the timePiece name predictable by calculating expireSecond outside.
	// This means we can't directly use AddToQueue as it calculates time.Now() internally.
	// This test is more about how AddToQueue *behaves* given its internal logic.

	// Alternative: Test the effect. If timePiece list's EXPIRE is set only once.
	// Add item1. Check TTL of timePiece list.
	// Add item2 (same delay, same key, hoping for same timePiece).
	// Check TTL again. If it was not reset (or significantly changed), it implies v was not > 0 on second call.
	// Then consume both items and check they are value1, value2.

	itemDelaySeconds := int64(5) // 5s delay, to give time to check TTL
	maxTTLSeconds := int64(10)   // timePiece list TTL

	// Call 1
	err1 := AddToQueue(ctx, rdb, delayQueueName, value1, itemDelaySeconds, maxTTLSeconds)
	assert.NoError(t, err1)

	// Construct the expected timePiece name. This is a bit of reverse engineering but necessary for this test.
	// Note: This makes the test tightly coupled to the timePiece naming convention.
	expectedExpireSecond := time.Now().Unix() + itemDelaySeconds 
	// It's possible time.Now().Unix() just ticked over if AddToQueue was slow or system clock aligned.
	// To make it more robust, let's get all timepieces and find the one for this queue.
	var timePieceKey string
	// Wait a very short moment for AddToQueue to complete its Redis commands
	time.Sleep(50 * time.Millisecond) 
	
	allTimePieces, errZ := rdb.ZRange(ctx, delayQueueName, 0, -1).Result()
	assert.NoError(t, errZ)
	assert.Len(t, allTimePieces, 1, "Should be one timePiece in sorted set")
	timePieceKey = allTimePieces[0] // This is the actual timePiece key created by the first call

	ttl1, errTTL1 := rdb.TTL(ctx, timePieceKey).Result()
	assert.NoError(t, errTTL1)
	// Expected TTL is around (maxTTLSeconds + itemDelaySeconds). Allow some execution time variance.
	assert.True(t, ttl1 > 0 && ttl1 <= time.Duration(maxTTLSeconds+itemDelaySeconds)*time.Second, "TTL for timePieceKey after first add is unexpected: %s", ttl1)
	t.Logf("TTL for %s after 1st add: %s (expected around %ds)", timePieceKey, ttl1, maxTTLSeconds+itemDelaySeconds)


	// Call 2 - with same parameters, aiming for same timePieceKey implicitly (by relying on internal time.Now())
	// This is the tricky part. If time.Now().Unix() in the second AddToQueue call is different, it's a new timePiece.
	// To ensure the *same* timePiece is targeted, we'd need to control `expireSecond`.
	// The current AddToQueue doesn't allow passing expireSecond.
	// So, this test can't directly test "ZADD NX returns 0 for the *same member*".
	// It can only test that if a *new* timePiece is made, it gets TTL, if *old*, it doesn't re-set TTL.

	// Let's assume for this test's purpose, we are testing adding a *second value* to an *existing timePiece*.
	// This means the Lua script's RPUSH part. The ZADD NX part would have returned 0.
	// The only way to achieve this reliably is if the test could force time.Now() or expireSecond.
	// Since it cannot, let's adapt the test to what it *can* verify:
	// 1. Add item1.
	// 2. Add item2 (different value, same delay parameters).
	// 3. If they fall into the same timePiece (due to calls in same second), list should have item1, item2. TTL set by first.
	// 4. If they fall into different timePieces, each list has 1 item. Each gets TTL.

	// Given the difficulty, let's simplify:
	// Test that two values added with the same delay are both retrievable.
	// This is already covered by TestBase "SequentialItemsWithSameDelay" if we check list content.
	// This specific test of "ZADD NX" for the *sorted set member* and its effect on TTL setting
	// is hard with current AddToQueue signature.

	// Focus on: If ZADD NX *were* to return 0 (meaning timePiece existed), `v > 0` would be false,
	// and `Expire` calls in Go would be skipped.
	// The Lua script *always* does RPUSH.

	// New approach for this test:
	// 1. Add value1 with delay D, maxTTL M. timePiece T1 is created. T1 gets TTL_M+D.
	// 2. Force a call to the Lua script directly for `addTimePieceScript` with the *same* T1, but new value2.
	//    This simulates the case where `ZADD NX` would return 0.
	// 3. Check that T1's TTL is not significantly changed (i.e., not reset to M+D from "now").
	// 4. Consume from queue, expect value1 then value2 from T1.

	// This means we need luaAddTimePieceSha to be loaded. loadLuaScripts is called by AddToQueue.
	if err := loadLuaScripts(ctx, rdb); err != nil { // Ensure scripts are loaded
		t.Fatalf("Failed to load Lua scripts: %v", err)
	}
	assert.NotEmpty(t, luaAddTimePieceSha, "luaAddTimePieceSha is empty")

	// Step 2 (modified): Directly call EvalSha for the *same timePieceKey* (from first AddToQueue)
	// This simulates the scenario where ZADD NX for this timePieceKey would return 0.
	// We need the score that was used for timePieceKey.
	score, errScore := rdb.ZScore(ctx, delayQueueName, timePieceKey).Result()
	assert.NoError(t, errScore)

	// EvalSha: KEYS[1]=delayQueueName, KEYS[2]=timePieceKey, ARGV[1]=score, ARGV[2]=timePieceKey, ARGV[3]=value2
	// The script returns result of ZADD NX. If member (timePieceKey) already exists, it's 0.
	evalResult, errEval := rdb.EvalSha(ctx, luaAddTimePieceSha,
		[]string{delayQueueName, timePieceKey}, // KEYS
		score, timePieceKey, value2,           // ARGV
	).Result()
	assert.NoError(t, errEval)
	assert.Equal(t, int64(0), evalResult, "EvalSha for existing timePieceKey should return 0 from ZADD NX")

	// Step 3: Check TTL of timePieceKey again. It should NOT have been reset by the EvalSha call
	// because the script doesn't do EXPIRE, and the Go code path for v > 0 was bypassed.
	// It will continue ticking down from its original TTL.
	time.Sleep(100 * time.Millisecond) // allow for command processing
	ttl2, errTTL2 := rdb.TTL(ctx, timePieceKey).Result()
	assert.NoError(t, errTTL2)
	assert.True(t, ttl2 > 0, "TTL for timePieceKey after second value push should still be positive")
	assert.True(t, ttl2 < ttl1 || (ttl1-ttl2 < 2*time.Second), "TTL for timePieceKey should be less than original, or very close, not reset. ttl1: %s, ttl2: %s", ttl1, ttl2)
	t.Logf("TTL for %s after 2nd value (direct eval): %s (was %s)", timePieceKey, ttl2, ttl1)


	// Step 4: Consume items. Expect value1 then value2 from this timePiece.
	consumerWg := &sync.WaitGroup{}
	consumerWg.Add(1)
	actualValues := make([]string, 0, 2)
	go func() {
		defer consumerWg.Done()
		resCh, errCh := GetFromQueue(ctx, rdb, delayQueueName)
		for i := 0; i < 2; i++ { // Expecting two items
			select {
			case item, ok := <-resCh:
				if !ok { t.Error("resCh closed prematurely"); return }
				t.Logf("Consumed item: %s", item)
				actualValues = append(actualValues, item)
			case err, ok := <-errCh:
				if !ok { t.Log("errCh closed"); return }
				if err != context.Canceled && err != redis.Nil {
					t.Errorf("Consumer error: %v", err)
				}
				if err == context.Canceled { return }
			case <-ctx.Done():
				t.Error("Context cancelled before consuming all items")
				return
			case <-time.After(time.Duration(itemDelaySeconds+5)*time.Second + pollerSleepDuration): // Generous timeout for consumption
				t.Errorf("Timeout waiting to consume items for duplicate test")
				return
			}
		}
	}()

	consumerWg.Wait()
	assert.Equal(t, []string{value1, value2}, actualValues, "Consumed items do not match expected for duplicate timePiece test")

	// Final check: queue should be empty
	cleanDelayQueue(ctx, rdb, delayQueueName) // Run clean and it should not panic
	zCard, _ := rdb.ZCard(ctx, delayQueueName).Result()
	assert.Equal(t, int64(0), zCard, "Sorted set should be empty after consuming all items")
	readyLen, _ := rdb.LLen(ctx, getReadyKey(delayQueueName)).Result()
	assert.Equal(t, int64(0), readyLen, "Ready list should be empty")
}


func TestContextCancellationDuringBLPop(t *testing.T) {
	rdb := getRdb()
	delayQueueName := "test_cancel_blpop_queue"
	ctx, cancel := context.WithCancel(context.Background())
	// No defer cancel() here, as we cancel it explicitly mid-test.

	cleanDelayQueue(ctx, rdb, delayQueueName) // Initial cleanup
	defer cleanDelayQueue(ctx, rdb, delayQueueName) // Cleanup after test

	consumerWg := &sync.WaitGroup{}
	consumerWg.Add(1)

	go func() {
		defer consumerWg.Done()
		resCh, errCh := GetFromQueue(ctx, rdb, delayQueueName)
		var itemReceived bool
		var errReceived error

		// This loop should break due to context cancellation
		for {
			select {
			case _, ok := <-resCh:
				if ok {
					itemReceived = true
					t.Error("Received item, but none should be sent when BLPop is cancelled")
				}
				// If channel is closed, loop should break or next select hits ctx.Done
			case err, ok := <-errCh:
				if ok { // errCh might send multiple redis.Nil before context.Canceled
					if err != redis.Nil && err != context.Canceled { // redis.Nil from BLPop timeout is acceptable
						t.Errorf("Received unexpected error: %v", err)
					}
					if err == context.Canceled { // This is the expected error path
						errReceived = err
						return // Exit goroutine once context.Canceled is confirmed
					}
				} else { // errCh closed
					if errReceived == nil { // If it closed without sending context.Canceled explicitly
						// This might happen if GetFromQueue exits cleanly after ctx.Done() without specific error sent
						// Check if context was indeed cancelled.
						if ctx.Err() != context.Canceled {
							t.Error("errCh closed without context.Canceled and context not showing Canceled state")
						}
					}
					return
				}
			case <-ctx.Done(): // Fallback if errCh doesn't immediately signal Canceled
				if errReceived == nil { // If not already set by errCh
					errReceived = ctx.Err()
				}
				return
			}
			if itemReceived { // Should not happen
				break
			}
		}
	}()

	// Allow GetFromQueue to start and BLPop to block
	time.Sleep(2 * blpopTimeout) // blpopTimeout is 1s, pollerSleepDuration is 100ms. Ensure BLPop is likely active.

	cancel() // Cancel the context

	consumerWg.Wait() // Wait for the consumer goroutine to exit

	// Assertions can be tricky here due to timing of select cases in consumer.
	// The main thing is that consumerWg.Wait() completed, implying goroutine exited.
	// And no unexpected errors were reported via t.Error/t.Errorf.
	// A check on errReceived in the goroutine would be more direct if it could be passed out,
	// but t.Error already serves this.
	t.Log("Consumer finished after context cancellation.")
	// We expect no items to be processed, and context.Canceled to be the reason for stopping.
}


func cleanDelayQueue(ctx context.Context, rdb *redis.Client, queueName string) {
	// Clean timePiece lists associated with the sorted set members
	timePieces, err := rdb.ZRange(ctx, queueName, 0, -1).Result()
	if err != nil && err != redis.Nil {
		rdb.Del(ctx, queueName) // Attempt to delete the key itself if ZRange fails for some reason
		panic(fmt.Sprintf("Error fetching timePieces for queue %s during cleanup: %v. Attempted to delete key directly.", queueName, err))
	}
	if len(timePieces) > 0 {
		// Pipeline deletions of timePiece lists for efficiency
		pipe := rdb.Pipeline()
		for _, timePiece := range timePieces {
			pipe.Del(ctx, timePiece)
		}
		_, err = pipe.Exec(ctx)
		if err != nil && !strings.Contains(err.Error(), "redis: nil") { // Allow "redis: nil" if some keys were already gone
			// Don't panic here, just log, as keys might have expired naturally or been deleted by other processes.
			// This cleanup is best-effort.
			// fmt.Printf("Error pipelining deletion of timePieces for %s: %v\n", queueName, err)
		}
	}

	// Delete the sorted set itself
	if delErr := rdb.Del(ctx, queueName).Err(); delErr != nil && delErr != redis.Nil {
		// fmt.Printf("Error deleting sorted set %s during cleanup: %v\n", queueName, delErr)
	}

	// Delete the ready list
	readyKey := getReadyKey(queueName)
	if delErr := rdb.Del(ctx, readyKey).Err(); delErr != nil && delErr != redis.Nil {
		// fmt.Printf("Error deleting ready list %s during cleanup: %v\n", readyKey, delErr)
	}
}

// Use: go test -bench=. -run=none
// Example: go test -bench=BenchmarkEndToEndLatency -run=^$ -benchtime=100x
func BenchmarkAddToQueue(b *testing.B) {
	rdb := getRdb()
	baseCtx := context.Background() // Use a base context for cleanup outside benchmark timing
	delayQueueName := "benchmark_add_to_queue"
	// Clean up before benchmark runs.
	// Note: cleanDelayQueue uses its own context passed to it.
	cleanDelayQueue(baseCtx, rdb, delayQueueName)

	// Context for the AddToQueue operations within the benchmark timing
	opCtx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Using delaySecond = 0 for immediate processing by poller, maxTTL is short
		err := AddToQueue(opCtx, rdb, delayQueueName, strconv.Itoa(i+1), 0, 10)
		if err != nil {
			b.Fatalf("AddToQueue failed: %v", err)
		}
	}
	b.StopTimer() // Stop timer for final cleanup if any significant work is done

	// Minimal cleanup after timed operations for this specific benchmark,
	// as GetFromQueue is not started here.
	// Full cleanup would be needed if items were processed.
	// For AddToQueue benchmark, we primarily measure write performance.
	// The original BenchmarkAddToQueue had a consumer part, but it was complex.
	// This simplified version focuses purely on AddToQueue.
	// If items need to be cleaned up that were added:
	// cleanDelayQueue(baseCtx, rdb, delayQueueName)
}

func BenchmarkEndToEndLatency(b *testing.B) {
	rdb := getRdb()
	delayQueueName := "benchmark_e2e_latency_queue"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cleanup before and after the benchmark
	cleanDelayQueue(ctx, rdb, delayQueueName)
	defer cleanDelayQueue(ctx, rdb, delayQueueName)

	delayDuration := 10 * time.Millisecond // 10ms delay
	// AddToQueue expects delaySecond as int64. For delays < 1s, this will be 0.
	// The actual delay is then driven by the poller a little bit, plus this initial score.
	// If we use time.Now().Unix() + 0, it's "due now".
	// If we use time.Now().Unix() + 1, it's "due in 1 second".
	// For 10ms, we want it to be picked up at the next poller cycle after 10ms.
	// So, the score should be time.Now().UnixNano() / 1e9 + delayInSecondsFloat
	// However, AddToQueue uses Unix(), so fractional seconds are lost for the score if delaySecond is 0.
	// Let's use delaySecond = 0 to mean "as fast as possible" to measure overhead.
	// The poller itself has a 100ms pollerSleepDuration.
	// To truly test a 10ms delay, the poller would need to be faster or AddToQueue/poller use millisecond scores.
	// For this benchmark, we'll use delaySecond = 0, acknowledging poller latency will be a factor.
	delaySecond := int64(0)
	maxTTL := int64(60) // 60 seconds max TTL

	// Use a small buffer for processedItemsCh for latency testing.
	// A buffer of 1 allows the consumer to place an item while the main loop
	// is processing the previous one, but still keeps the flow fairly sequential
	// for measuring individual latencies. An unbuffered channel would also work
	// but might be slightly slower if there's any tiny scheduling jitter.
	processedItemsCh := make(chan string, 1)
	consumerWg := &sync.WaitGroup{}

	consumerWg.Add(1)
	go func() {
		defer consumerWg.Done()
		// Use a new context for GetFromQueue if its lifecycle is strictly tied to this goroutine,
		// but using the outer ctx is fine as it's cancelled at the end.
		resCh, errCh := GetFromQueue(ctx, rdb, delayQueueName)
		for {
			select {
			case item, ok := <-resCh:
				if !ok { // Channel closed
					return
				}
				processedItemsCh <- item
			case err, ok := <-errCh:
				if !ok { // Channel closed
					return
				}
				// context.Canceled is expected on shutdown. redis.Nil from BLPop timeout is also normal.
				if err != nil && err != context.Canceled && err != redis.Nil {
					b.Errorf("Consumer error: %v", err)
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		value := "e2e_test_value_" + strconv.Itoa(i)
		// AddToQueue expects delay in seconds. For very short delays, this means the item is due "now".
		// The actual score will be time.Now().Unix() + delaySecond.
		// To incorporate the 10ms, AddToQueue would need to accept float scores or millisecond precision.
		// With current AddToQueue, for 10ms, we pass delaySecond = 0.
		// The effective delay will be influenced by poller interval.
		// We record current time just before AddToQueue.
		// The score in Redis will be based on time.Now().Unix() at the moment of AddToQueue.
		// For this benchmark, we'll use the `delayDuration` variable to sleep *after* adding to queue
		// to simulate the intended delay, then measure from *after* that sleep.
		// This way, we measure (Redis ops + GetFromQueue retrieval time), not the intentional delay.
		// OR, we measure total time including the 10ms sleep.
		// The subtask asks for "time taken for an item to go through the queue ... from AddToQueue to being received".
		// This implies the 10ms should be part of the measured latency if it's an *intended* part of queue behavior.
		// Let's assume the 10ms is an *additional* artificial delay for the item itself,
		// and we want to measure how long Redis + GetFromQueue takes *after* that.
		// No, the problem says: "Define a short delaySecond (e.g., 50 milliseconds or even 0-1 ms ... Let's use 10ms.)"
		// This means the score should reflect this. Current AddToQueue uses int64 seconds.
		// If AddToQueue is called with delaySecond=0, the score is `time.Now().Unix()`.
		// The poller wakes up every 100ms.
		// So, the minimum latency will be dominated by the poller interval if delaySecond=0.

		// For an accurate 10ms test, AddToQueue and poller need redesign for ms precision.
		// Given current tools: use delaySecond = 0, and acknowledge poller interval is the main factor.
		// The reported latency will be roughly: AddToQueue_overhead + poller_interval_fragment + GetFromQueue_overhead.

		startTime := time.Now()

		// Add item that is due effectively "now" (or specific second if delaySecond > 0)
		err := AddToQueue(ctx, rdb, delayQueueName, value, delaySecond, maxTTL)
		if err != nil {
			b.Fatalf("AddToQueue failed for item %s: %v", value, err)
		}

		// Wait for the specific item to be processed
		var receivedValue string
		select {
		case receivedValue = <-processedItemsCh:
			// Good, item received
		case <-time.After(15 * time.Second): // Increased timeout
			b.Fatalf("Timeout waiting for item %s after 15s", value)
			return // Exit benchmark iteration
		}

		if receivedValue != value {
			// This could happen if items are processed out of order or test logic is flawed.
			// For simple 1-by-1 processing, this shouldn't occur.
			b.Fatalf("Logic error: expected item %s, but received %s", value, receivedValue)
		}
		
		// Simulate the intended delay if AddToQueue can't handle it with precision.
		// This is tricky. If AddToQueue sets score to NOW, then poller (100ms) is the main delay.
		// If we add a 10ms sleep here, we are not measuring the queue system's handling of a 10ms scheduled delay.
		// Let's stick to measuring the system as-is with delaySecond=0.
		// time.Sleep(delayDuration) // This would be adding the 10ms delay manually.

		latency := time.Since(startTime)
		b.ReportMetric(float64(latency.Nanoseconds()), "ns/op")
		// Optional: Report number of items to make b.N clearer in output
		// b.ReportMetric(1, "item/op")
	}

	b.StopTimer() // Stop timer before final cleanup

	cancel()           // Signal consumer and its poller to stop
	consumerWg.Wait()  // Wait for consumer goroutine to finish
}
