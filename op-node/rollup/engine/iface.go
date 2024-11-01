package engine

import (
	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

// EngineState provides a read-only interface of the forkchoice state properties of the L2 Engine.
type EngineState interface {
	// Finalized 获取最终确认的 L2 区块引用。
	Finalized() eth.L2BlockRef
	// UnsafeL2Head 获取不安全的 L2 头部。
	UnsafeL2Head() eth.L2BlockRef
	// SafeL2Head 获取安全的 L2 头部。
	SafeL2Head() eth.L2BlockRef
}

type Engine interface {
	ExecEngine
	derive.L2Source
}

type LocalEngineState interface {
	EngineState
	// PendingSafeL2Head 获取待定的安全 L2 头部。
	PendingSafeL2Head() eth.L2BlockRef
	// BackupUnsafeL2Head 获取备份的不安全 L2 头部。
	BackupUnsafeL2Head() eth.L2BlockRef
}

type LocalEngineControl interface {
	LocalEngineState
	ResetEngineControl
}

var _ LocalEngineControl = (*EngineController)(nil)
