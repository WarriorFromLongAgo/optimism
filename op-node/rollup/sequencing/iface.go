package sequencing

import (
	"context"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum-optimism/optimism/op-node/rollup/event"
)

type SequencerIface interface {
	event.Deriver
	// NextAction returns when the sequencer needs to do the next change, and iff it should do so.
	// 返回排序器需要执行下一个动作的时间，以及是否应该执行。用于调度排序器的操作。
	NextAction() (t time.Time, ok bool)
	// Active 返回排序器当前是否处于活跃状态。
	Active() bool
	// Init 初始化排序器，可以设置其初始状态为活跃或非活跃。
	Init(ctx context.Context, active bool) error
	// Start 启动排序器，指定从哪个区块头开始工作。
	Start(ctx context.Context, head common.Hash) error
	// Stop 停止排序器，返回停止时的最新区块哈希。
	Stop(ctx context.Context) (hash common.Hash, err error)
	// SetMaxSafeLag 设置最大安全滞后值，控制排序器与安全头部的最大允许差距。
	SetMaxSafeLag(ctx context.Context, v uint64) error
	// OverrideLeader 覆盖领导者设置，可能用于强制成为领导者。
	OverrideLeader(ctx context.Context) error
	// ConductorEnabled 检查排序器的指挥器（Conductor）是否启用。
	ConductorEnabled(ctx context.Context) bool
	// Close 关闭排序器，释放资源。
	Close()
}
