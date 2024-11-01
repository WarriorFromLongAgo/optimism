package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

type BuildStartEvent struct {
	Attributes *derive.AttributesWithParent
}

func (ev BuildStartEvent) String() string {
	return "build-start"
}

func (eq *EngDeriver) onBuildStart(ev BuildStartEvent) {
	ctx, cancel := context.WithTimeout(eq.ctx, buildStartTimeout)
	defer cancel()

	// 检查是否存在小型重组
	// 是指在区块链中发生的短距离分叉和重新组织。在 Optimism 的上下文中，这通常涉及 L2（Layer 2）链的重新组织，可能是由于与 L1（Layer 1）链同步过程中的一些不一致导致的。
	// 范围有限：通常只涉及少量最近的区块。
	//临时性：这种重组通常很快就会解决，系统会自动调整到正确的状态。
	//影响较小：对整体网络的影响通常较小，但可能会导致一些交易被临时回滚或重新排序。
	// 通过比较新区块的属性和当前的待定安全头部来检测可能的重组：
	if ev.Attributes.DerivedFrom != (eth.L1BlockRef{}) &&
		eq.ec.PendingSafeL2Head().Hash != ev.Attributes.Parent.Hash {
		// 警告可能发生的小型重组
		// Warn about small reorgs, happens when pending safe head is getting rolled back
		eq.log.Warn("block-attributes derived from L1 do not build on pending safe head, likely reorg",
			"pending_safe", eq.ec.PendingSafeL2Head(), "attributes_parent", ev.Attributes.Parent)
	}
	// 准备分叉选择更新事件
	// 在 onBuildStart 方法中添加 ForkchoiceUpdateEvent 的主要原因是为了确保系统在开始构建新区块时，所有相关组件都有最新的链状态信息。
	// 同步状态：当开始构建新区块时，需要确保系统的其他部分知道当前的不安全头部、安全头部和最终确认头部。这对于维护整个系统的一致性至关重要。
	// 处理潜在的重组：如果在开始构建新区块时检测到可能的重组，这个事件可以帮助系统快速调整到正确的状态。
	// 优化性能：通过在构建开始时就发出这个事件，可以避免其他组件在之后需要单独查询这些信息，从而提高系统效率。
	// 触发后续操作：其他组件可能需要根据最新的分叉选择状态来执行特定的操作或更新自己的状态。
	// 通过在区块构建开始时发出这个事件，系统确保了所有相关组件都能及时获得最新的链状态信息，从而保持整个系统的一致性和效率。
	fcEvent := ForkchoiceUpdateEvent{
		UnsafeL2Head:    ev.Attributes.Parent,
		SafeL2Head:      eq.ec.safeHead,
		FinalizedL2Head: eq.ec.finalizedHead,
	}
	// 验证不安全头部不在最终头部之后
	if fcEvent.UnsafeL2Head.Number < fcEvent.FinalizedL2Head.Number {
		err := fmt.Errorf("invalid block-building pre-state, unsafe head %s is behind finalized head %s", fcEvent.UnsafeL2Head, fcEvent.FinalizedL2Head)
		eq.emitter.Emit(rollup.CriticalErrorEvent{Err: err}) // make the node exit, things are very wrong.
		return
	}
	// 创建分叉选择状态
	// 是一个重要的数据结构，用于表示区块链的分叉选择状态。它包含了三个关键的区块哈希：
	// HeadBlockHash：当前链的头部区块哈希
	// SafeBlockHash：当前认为安全的区块哈希
	// FinalizedBlockHash：已经最终确认的区块哈希

	// 同步状态：它提供了一个快照，显示了链的当前状态，包括不安全、安全和最终确认的头部。
	// 更新执行层：它被用来通知执行客户端（如 Geth）关于最新的链状态，特别是在分叉选择更新（Forkchoice Update）操作中。
	// 一致性维护：通过传播这个状态，确保网络中的所有节点对链的当前状态有一致的理解。
	// 分叉处理：在发生链重组时，这个结构可以帮助系统快速调整到正确的链上。
	fc := eth.ForkchoiceState{
		HeadBlockHash:      fcEvent.UnsafeL2Head.Hash,
		SafeBlockHash:      fcEvent.SafeL2Head.Hash,
		FinalizedBlockHash: fcEvent.FinalizedL2Head.Hash,
	}
	// 记录构建开始时间
	buildStartTime := time.Now()
	// 启动新的有效载荷构建
	id, errTyp, err := startPayload(ctx, eq.ec.engine, fc, ev.Attributes.Attributes)
	if err != nil {
		// 处理不同类型的错误
		switch errTyp {
		case BlockInsertTemporaryErr:
			// 临时错误，可以稍后重试
			// RPC errors are recoverable, we can retry the buffered payload attributes later.
			eq.emitter.Emit(rollup.EngineTemporaryErrorEvent{Err: fmt.Errorf("temporarily cannot insert new safe block: %w", err)})
			return
		case BlockInsertPrestateErr:
			// 需要重置以解决预状态问题
			eq.emitter.Emit(rollup.ResetEvent{Err: fmt.Errorf("need reset to resolve pre-state problem: %w", err)})
			return
		case BlockInsertPayloadErr:
			// 构建无效
			eq.emitter.Emit(BuildInvalidEvent{Attributes: ev.Attributes, Err: err})
			return
		default:
			// 未知错误类型
			eq.emitter.Emit(rollup.CriticalErrorEvent{Err: fmt.Errorf("unknown error type %d: %w", errTyp, err)})
			return
		}
	}
	eq.emitter.Emit(fcEvent)

	eq.emitter.Emit(BuildStartedEvent{
		Info:         eth.PayloadInfo{ID: id, Timestamp: uint64(ev.Attributes.Attributes.Timestamp)},
		BuildStarted: buildStartTime,
		IsLastInSpan: ev.Attributes.IsLastInSpan,
		DerivedFrom:  ev.Attributes.DerivedFrom,
		Parent:       ev.Attributes.Parent,
	})
}
