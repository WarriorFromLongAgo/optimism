package status

import (
	"context"

	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum-optimism/optimism/op-node/rollup/event"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

// L1Tracker implements the L1Fetcher interface while proactively maintaining a reorg-aware cache
// of L1 block references by number. This handles the L1UnsafeEvent in order to populate the cache with
// the latest L1 block references.
type L1Tracker struct {
	derive.L1Fetcher
	cache *l1HeadBuffer
}

func NewL1Tracker(inner derive.L1Fetcher) *L1Tracker {
	return &L1Tracker{
		L1Fetcher: inner,
		cache:     newL1HeadBuffer(1000),
	}
}

// OnEvent 返回 true，表示事件已被处理。
func (st *L1Tracker) OnEvent(ev event.Event) bool {
	switch x := ev.(type) {
	case L1UnsafeEvent:
		//它会调用 st.cache.Insert(x.L1Unsafe) 方法，将 L1UnsafeEvent 中的 L1Unsafe 信息插入到 L1Tracker 的缓存中。
		st.cache.Insert(x.L1Unsafe)
	default:
		return false
	}

	return true
}

// L1BlockRefByNumber 在 L1Tracker 自身的 L1BlockRefByNumber 方法中使用。当需要获取特定编号的 L1 区块引用时，会首先检查缓存：
func (l *L1Tracker) L1BlockRefByNumber(ctx context.Context, num uint64) (eth.L1BlockRef, error) {
	if ref, ok := l.cache.Get(num); ok {
		return ref, nil
	}

	return l.L1Fetcher.L1BlockRefByNumber(ctx, num)
}
