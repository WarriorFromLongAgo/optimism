package engine

import (
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

type PayloadSuccessEvent struct {
	// if payload should be promoted to safe (must also be pending safe, see DerivedFrom)
	IsLastInSpan bool
	// payload is promoted to pending-safe if non-zero
	DerivedFrom eth.L1BlockRef

	Envelope *eth.ExecutionPayloadEnvelope
	Ref      eth.L2BlockRef
}

func (ev PayloadSuccessEvent) String() string {
	return "payload-success"
}

// 处理执行载荷成功事件
func (eq *EngDeriver) onPayloadSuccess(ev PayloadSuccessEvent) {
	// 发出提升不安全区块事件
	// 将区块状态从未确认提升为不安全状态
	eq.emitter.Emit(PromoteUnsafeEvent{Ref: ev.Ref})

	// 如果区块是从 L1 派生的，则可以将其视为（待定）安全
	// 检查是否有关联的 L1 区块引用
	// If derived from L1, then it can be considered (pending) safe
	if ev.DerivedFrom != (eth.L1BlockRef{}) {
		// 发出提升待定安全事件
		// IsLastInSpan 表示是否是当前 span 中的最后一个区块
		eq.emitter.Emit(PromotePendingSafeEvent{
			Ref:         ev.Ref,
			Safe:        ev.IsLastInSpan,
			DerivedFrom: ev.DerivedFrom,
		})
	}
	// 获取执行载荷信息并记录日志
	payload := ev.Envelope.ExecutionPayload
	eq.log.Info("Inserted block", "hash", payload.BlockHash, "number", uint64(payload.BlockNumber),
		"state_root", payload.StateRoot, "timestamp", uint64(payload.Timestamp), "parent", payload.ParentHash,
		"prev_randao", payload.PrevRandao, "fee_recipient", payload.FeeRecipient,
		"txs", len(payload.Transactions), "last_in_span", ev.IsLastInSpan, "derived_from", ev.DerivedFrom)

	// 发出尝试更新引擎事件
	// 触发引擎状态的更新
	eq.emitter.Emit(TryUpdateEngineEvent{})
}
