# golang redis delay queue
## Usage
`import delay "github.com/yaodongen/delay-queue"`

## Sample 
```go
package main

import (
	"context"
	"github.com/go-redis/redis/v8"
	delay "github.com/yaodongen/delay-queue"
)

func main() {
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
	// producer
	err := delay.AddToDelayQueue(ctx, rdb, "key", "123", 5, 86400)
	if err != nil {
		// your own logic
	}

	// consumer
	go func() {
		resCh, errCh := delay.GetFromDelayQueue(ctx, rdb, "key")
		for res := range resCh {
			// your own logic
			_ = res
		}
		for err := range errCh {
			if err != context.Canceled && err != context.DeadlineExceeded {
				// your own logic
			}
		}
	}()

}
```

## Redis Delay Queue Main Logic
### Push
1. add the timePiece(sample: "1645614542") to sorted set 
2. rpush the real data to timePiece

### Pop
1. get a timePiece from sorted set which is before time.Now()
2. lpop the real data from timePiece

## Redis 延迟队列原理
### 入队列
1. 把时间片(样例: "1645614542") 添加到 zset 中
2. 把需要存储的数据 rpush 到时间片中

### 出队列
1. 从 zset 中取出早于当前时间戳的一个时间片
2. 从时间片中 lpop 对应的数据


