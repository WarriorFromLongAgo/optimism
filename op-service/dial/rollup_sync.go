package dial

import (
	"context"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/log"
)

// WaitRollupSync 用于等待 Rollup 客户端同步到指定的 L1 区块目标。
func WaitRollupSync(
	ctx context.Context,
	lgr log.Logger,
	// Rollup 客户端接口，提供同步状态的方法。
	rollup SyncStatusProvider,
	// 目标 L1 区块号，等待 Rollup 客户端同步到该区块。
	l1BlockTarget uint64,
	// 轮询间隔时间，控制轮询的频率。
	pollInterval time.Duration,
) error {
	// 主要功能是轮询 Rollup 客户端的同步状态，直到其同步到指定的 L1 区块目标。
	// 如果在等待过程中上下文被取消，则方法会返回上下文取消的错误。

	timer := time.NewTimer(pollInterval)
	defer timer.Stop()

	for {
		// 调用 rollup.SyncStatus 方法获取 Rollup 客户端的同步状态。
		syncst, err := rollup.SyncStatus(ctx)
		if err != nil {
			// don't log assuming caller handles and logs errors
			return fmt.Errorf("getting sync status: %w", err)
		}

		// 使用日志记录器记录当前的 L1 区块号和目标 L1 区块号。
		lgr := lgr.With("current_l1", syncst.CurrentL1, "target_l1", l1BlockTarget)
		// 如果当前的 L1 区块号大于或等于目标 L1 区块号，记录日志并返回 nil 表示同步完成。
		if syncst.CurrentL1.Number >= l1BlockTarget {
			lgr.Info("rollup current L1 block target reached")
			return nil
		}

		// 如果当前的 L1 区块号小于目标 L1 区块号，记录日志并重置定时器。
		lgr.Info("rollup current L1 block still behind target, retrying")

		timer.Reset(pollInterval)
		select {
		// 如果定时器触发，继续下一次轮询。
		case <-timer.C: // next try
		// 如果上下文被取消，记录警告日志并返回上下文取消的错误。
		case <-ctx.Done():
			lgr.Warn("waiting for rollup sync timed out")
			return ctx.Err()
		}
	}
}
