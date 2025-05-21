package delay

import (
	"context"
	"fmt"
	"github.com/go-redis/redis/v8"
	"strconv"
	"sync"
	"time"
)

// TODO: make this configurable
const pollerSleepDuration = 100 * time.Millisecond
const blpopTimeout = 1 * time.Second
const zrangeLimit = 10 // Number of items to fetch in one poll

// Lua script to atomically ZREM from sorted set and RPUSH to list.
// KEYS[1] - Source sorted set (e.g., "myqueue")
// KEYS[2] - Destination list (e.g., "myqueue_ready")
// ARGV[1] - Member to move (timePiece)
// Returns 1 if successful, 0 otherwise.
const moveTimePieceToReadyListScript = `
local timePiece = ARGV[1]
local score = redis.call('ZSCORE', KEYS[1], timePiece)
if not score then
  return 0
end
local zrem_success = redis.call('ZREM', KEYS[1], timePiece)
if zrem_success == 0 then
  return 0
end
redis.call('RPUSH', KEYS[2], timePiece)
return 1
`

// Lua script to atomically ZADD NX to sorted set and RPUSH to list.
// KEYS[1] - Sorted set key (e.g., "myqueue")
// KEYS[2] - List key for the timePiece (e.g., "dq:myqueue:timestamp")
// ARGV[1] - Score for ZADD (expireSecond)
// ARGV[2] - Member for ZADD (timePiece string)
// ARGV[3] - Value to RPUSH to the list
// Returns the result of ZADD NX (1 if added, 0 if element already exists).
const addTimePieceScript = `
local zadd_result = redis.call('ZADD', KEYS[1], ARGV[1], ARGV[2], 'NX')
redis.call('RPUSH', KEYS[2], ARGV[3])
return zadd_result
`

var (
	luaMoveTimePieceSha string
	luaAddTimePieceSha  string
	luaLoadOnce         sync.Once
)

// loadLuaScripts ensures Lua scripts are loaded into Redis and stores their SHAs.
// This should be called once, ideally during application initialization if the poller
// is long-lived or started frequently. For now, we can call it from the poller itself
// with a sync.Once guard.
func loadLuaScripts(ctx context.Context, rdb *redis.Client) error {
	var err error
	luaLoadOnce.Do(func() {
		// Load script for poller
		luaMoveTimePieceSha, err = rdb.ScriptLoad(ctx, moveTimePieceToReadyListScript).Result()
		if err != nil {
			// Log or handle error: e.g., log.Printf("Failed to load moveTimePieceToReadyListScript: %v", err)
			// This error might be critical for poller functionality.
			return // Potentially return the error to signal failure
		}

		// Load script for AddToQueue
		luaAddTimePieceSha, err = rdb.ScriptLoad(ctx, addTimePieceScript).Result()
		if err != nil {
			// Log or handle error: e.g., log.Printf("Failed to load addTimePieceScript: %v", err)
			// This error might be critical for AddToQueue functionality.
			// The error from the first ScriptLoad is shadowed if not returned earlier.
		}
	})
	// Return the last error encountered, or nil if all successful.
	// A more robust error handling might collect all errors.
	return err
}

func getReadyKey(key string) string {
	return key + "_ready"
}

// poller moves due items from the sorted set (key) to a ready list (key_ready).
func poller(ctx context.Context, rdb *redis.Client, key string, readyKey string) {
	// Use a WaitGroup to ensure we know when the poller has fully exited if needed.
	// For now, it's mainly for good practice in case we expand its shutdown logic.
	var wg sync.WaitGroup
	wg.Add(1)
	defer wg.Done()

	// Attempt to load Lua scripts. If this fails, the poller might not operate correctly.
	// A robust application might have a global init step for this.
	if err := loadLuaScripts(ctx, rdb); err != nil {
		// Log or handle critical error: Lua script loading failed.
		// fmt.Printf("Poller for key %s: Failed to load Lua scripts: %v. Poller will not run.\n", key, err)
		// Depending on policy, could signal error back or simply not start.
		// For now, if scripts don't load, EvalSha will fail later.
	}


	ticker := time.NewTicker(pollerSleepDuration)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().Unix()
			opt := redis.ZRangeBy{
				Min:    "0",
				Max:    strconv.FormatInt(now, 10),
				Offset: 0,
				Count:  zrangeLimit, // Use ZRANGEBYSCORE to get members
			}
			// We get ZWithKey structs which include members and their scores.
			// However, the current Lua script only needs the member.
			// Using ZRangeByScoreWithScores is fine, ZRangeByScore would also work if we only need members.
			results, err := rdb.ZRangeByScoreWithScores(ctx, key, &opt).Result()
			if err != nil {
				// Log error
				continue
			}

			if len(results) == 0 {
				continue
			}

			for _, z := range results {
				timePiece := z.Member.(string) // Assert Member is string, as stored
				// score := z.Score // Available if needed for more complex logic (e.g., re-add on failure)

				// Atomically move timePiece from sorted set to ready list using Lua script
				// Ensure luaMoveTimePieceSha is loaded.
				if luaMoveTimePieceSha == "" {
					// Script not loaded, log and skip. This indicates an issue with loadLuaScripts.
					// fmt.Printf("Lua script SHA for moveTimePieceToReadyListScript is empty. Skipping move for %s.\n", timePiece)
					continue;
				}

				// EvalSha executes the loaded script by its SHA hash.
				// KEYS: {key, readyKey}, ARGV: {timePiece}
				res, err := rdb.EvalSha(ctx, luaMoveTimePieceSha, []string{key, readyKey}, timePiece).Result()
				if err != nil {
					// Handle Lua script execution error.
					// This could be due to script error, Redis error, or if SHA wasn't found (e.g. SCRIPT FLUSH)
					// fmt.Printf("Error executing Lua script for timePiece %s: %v\n", timePiece, err)
					// If redis.Nil, it means script returned nil, which our script doesn't do.
					// If it's an error related to NOSCRIPT, we might need to reload the script.
					if redis.HasErrorPrefix(err, "NOSCRIPT") {
						// fmt.Printf("NOSCRIPT error for moveTimePieceToReadyListScript. Forcing reload.\n")
						// Reset loadOnce and try to load again. This is a basic retry.
						// A more robust solution might involve a central script manager.
						luaLoadOnce = sync.Once{} 
						if loadErr := loadLuaScripts(ctx, rdb); loadErr != nil {
							// fmt.Printf("Failed to reload Lua script: %v\n", loadErr)
						}
					}
					// Depending on the error, we might want to skip this item or retry.
					continue
				}

				// Our Lua script returns 1 for success, 0 for failure (e.g., item not found).
				if val, ok := res.(int64); !ok || val == 0 {
					// Item was not moved (e.g., already processed by another poller, or ZSCORE returned nil)
					// fmt.Printf("Lua script did not move timePiece %s (result: %v, ok: %v). May have been processed by another instance or did not exist.\n", timePiece, res, ok)
				}
				// If val == 1, it was successfully moved.
			}
		}
	}
}


// AddToQueue
// 1. add the timePiece(sample: "1645614542") to sorted set
// 2. rpush the real data to timePiece
//
// @delaySecond, the expected delay seconds, 600 means delay 600 second
// @maxTTL, the max time data will live if there is no consumer
func AddToQueue(ctx context.Context, rdb *redis.Client, key string, value string, delaySecond, maxTTL int64) error {
	// Ensure Lua scripts are loaded. Calling this here is safe due to sync.Once.
	// This is important if AddToQueue can be called before a poller starts, or in an environment without a poller.
	if loadErr := loadLuaScripts(ctx, rdb); loadErr != nil {
		// fmt.Printf("AddToQueue: Failed to load Lua scripts: %v. Aborting operation.\n", loadErr)
		return fmt.Errorf("failed to load Lua scripts: %w", loadErr)
	}
	if luaAddTimePieceSha == "" {
		// This case should ideally be covered by loadLuaScripts error handling,
		// but as a safeguard:
		// fmt.Printf("AddToQueue: luaAddTimePieceSha is empty. Scripts may not have loaded correctly.\n")
		return fmt.Errorf("luaAddTimePieceSha is not loaded; scripts unavailable")
	}

	expireSecond := time.Now().Unix() + delaySecond
	timePiece := fmt.Sprintf("dq:%s:%d", key, expireSecond)

	// Execute the Lua script to atomically ZADD NX and RPUSH.
	// KEYS: {key, timePiece}, ARGV: {expireSecond, timePiece, value}
	scriptResult, err := rdb.EvalSha(ctx, luaAddTimePieceSha,
		[]string{key, timePiece},
		expireSecond, timePiece, value,
	).Result()

	if err != nil {
		// Handle Lua script execution error.
		// Check for NOSCRIPT error to attempt reloading, similar to poller.
		if redis.HasErrorPrefix(err, "NOSCRIPT") {
			// fmt.Printf("NOSCRIPT error for addTimePieceScript. Forcing reload.\n")
			luaLoadOnce = sync.Once{} // Reset Once to allow re-loading
			if loadErr := loadLuaScripts(ctx, rdb); loadErr != nil {
				return fmt.Errorf("failed to reload addTimePieceScript after NOSCRIPT: %w", loadErr)
			}
			// Retry EvalSha after reload
			scriptResult, err = rdb.EvalSha(ctx, luaAddTimePieceSha,
				[]string{key, timePiece},
				expireSecond, timePiece, value,
			).Result()
			if err != nil {
				return fmt.Errorf("failed to execute addTimePieceScript after reload: %w", err)
			}
		} else {
			return fmt.Errorf("failed to execute addTimePieceScript: %w", err)
		}
	}

	v, ok := scriptResult.(int64)
	if !ok {
		// Script should return int64 (0 or 1). If not, it's an unexpected response.
		return fmt.Errorf("unexpected result type from addTimePieceScript: %T, value: %v", scriptResult, scriptResult)
	}

	// new timePiece will set expire time if it was newly added (v > 0)
	if v > 0 {
		rdb.Expire(ctx, timePiece, time.Second*time.Duration(maxTTL+delaySecond))
		// sorted set max live time (e.g., 3 days)
		rdb.Expire(ctx, key, time.Hour*24*3)
	}
	return nil // err was handled or is nil
}

// GetFromQueue
// 1. get a timePiece from sorted set which is before time.Now()
// 2. lpop the real data from timePiece
//
// Usage: Use it in a script or goroutine
func GetFromQueue(ctx context.Context, rdb *redis.Client, key string) (chan string, chan error) {
	resCh := make(chan string, 0) // unbuffered channel for results
	errCh := make(chan error, 1)   // buffered channel for error reporting

	readyKey := getReadyKey(key)

	// Start the poller goroutine for this key.
	// For simplicity, we start it unconditionally.
	// A more robust implementation would ensure only one poller runs per key.
	go poller(ctx, rdb, key, readyKey)

	go func() {
		defer close(resCh)
		defer close(errCh)
		for {
			select {
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			default:
				// BLPop blocks until an item is available or timeout
				results, err := rdb.BLPop(ctx, blpopTimeout, readyKey).Result()
				if err != nil {
					if err == redis.Nil {
						// Timeout, check context and loop again
						select {
						case <-ctx.Done():
							errCh <- ctx.Err()
							return
						default:
							continue
						}
					}
					// Actual error from BLPop
					errCh <- err
					return
				}

				// results[0] is the key name, results[1] is the value (timePiece)
				if len(results) < 2 {
					// Should not happen with BLPop but good to check
					continue
				}
				listK := results[1]

				// Loop to LPop all items from this timePiece list
				for {
					s, lpopErr := rdb.LPop(ctx, listK).Result()
					if lpopErr == nil {
						select {
						case resCh <- s:
						case <-ctx.Done():
							errCh <- ctx.Err()
							return
						}
					} else if lpopErr == redis.Nil {
						// The timePiece list is now empty, delete it.
						// ZRem was handled by the poller.
						rdb.Del(ctx, listK) // Delete the list key itself
						break               // Break from LPop loop, go to next BLPop
					} else {
						// Error during LPop
						errCh <- lpopErr
						return // Terminate the goroutine on LPop error
					}
				}
			}
		}
	}()
	return resCh, errCh
}
