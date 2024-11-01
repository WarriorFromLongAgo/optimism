package eth

import (
	"context"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
)

// HeadSignalFn is used as callback function to accept head-signals
type HeadSignalFn func(ctx context.Context, sig L1BlockRef)

type NewHeadSource interface {
	SubscribeNewHead(ctx context.Context, ch chan<- *types.Header) (ethereum.Subscription, error)
}

// WatchHeadChanges wraps a new-head subscription from NewHeadSource to feed the given Tracker.
// The ctx is only used to create the subscription, and does not affect the returned subscription.
func WatchHeadChanges(ctx context.Context, src NewHeadSource, fn HeadSignalFn) (ethereum.Subscription, error) {
	// 创建一个缓冲通道 headChanges 用于接收新的区块头。
	headChanges := make(chan *types.Header, 10)
	// 使用提供的 NewHeadSource 接口订阅新的区块头，将结果发送到 headChanges 通道。
	sub, err := src.SubscribeNewHead(ctx, headChanges)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		eventsCtx, eventsCancel := context.WithCancel(context.Background())
		defer sub.Unsubscribe()
		defer eventsCancel()

		// We can handle a quit signal while fn is running, by closing the ctx.
		go func() {
			select {
			case <-quit:
				eventsCancel()
			case <-eventsCtx.Done(): // don't wait for quit signal if we closed for other reasons.
				return
			}
		}()

		for {
			select {
			case header := <-headChanges:
				fn(eventsCtx, L1BlockRef{
					Hash:       header.Hash(),
					Number:     header.Number.Uint64(),
					ParentHash: header.ParentHash,
					Time:       header.Time,
				})
			case <-eventsCtx.Done():
				return nil
			case err := <-sub.Err(): // if the underlying subscription fails, stop
				return err
			}
		}
	}), nil
}

type L1BlockRefsSource interface {
	L1BlockRefByLabel(ctx context.Context, label BlockLabel) (L1BlockRef, error)
}

// PollBlockChanges opens a polling loop to fetch the L1 block reference with the given label,
// on provided interval and with request timeout. Results are returned with provided callback fn,
// which may block to pause/back-pressure polling.
// PollBlockChanges 打开一个轮询循环，以获取具有给定标签的 L1 块引用，
// 在提供的间隔和请求超时内。结果通过提供的回调 fn 返回，
// 可能会阻塞以暂停/背压轮询。
func PollBlockChanges(log log.Logger, src L1BlockRefsSource, fn HeadSignalFn,
	label BlockLabel, interval time.Duration, timeout time.Duration) ethereum.Subscription {
	return event.NewSubscription(func(quit <-chan struct{}) error {
		if interval <= 0 {
			log.Warn("polling of block is disabled", "interval", interval, "label", label)
			<-quit
			return nil
		}
		eventsCtx, eventsCancel := context.WithCancel(context.Background())
		defer eventsCancel()
		// We can handle a quit signal while fn is running, by closing the ctx.
		go func() {
			select {
			case <-quit:
				eventsCancel()
			case <-eventsCtx.Done(): // don't wait for quit signal if we closed for other reasons.
				return
			}
		}()

		// 创建一个定时器，按照指定的间隔触发。
		// 每次触发时，调用 L1BlockRefByLabel 方法获取指定标签（safe 或 finalized）的最新区块。
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				reqCtx, reqCancel := context.WithTimeout(eventsCtx, timeout)
				// 如果成功获取区块信息，则调用提供的回调函数 fn 处理新的区块信息。
				ref, err := src.L1BlockRefByLabel(reqCtx, label)
				reqCancel()
				if err != nil {
					// 如果获取区块失败，会记录警告日志但继续轮询。
					log.Warn("failed to poll L1 block", "label", label, "err", err)
				} else {
					fn(eventsCtx, ref)
				}
			case <-eventsCtx.Done():
				return nil
			}
		}
	})
}
