package engine

import (
	"context"
	"fmt"

	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

type PayloadProcessEvent struct {
	// if payload should be promoted to safe (must also be pending safe, see DerivedFrom)
	IsLastInSpan bool
	// payload is promoted to pending-safe if non-zero
	DerivedFrom eth.L1BlockRef

	Envelope *eth.ExecutionPayloadEnvelope
	Ref      eth.L2BlockRef
}

func (ev PayloadProcessEvent) String() string {
	return "payload-process"
}

// onPayloadProcess 处理执行载荷事件
func (eq *EngDeriver) onPayloadProcess(ev PayloadProcessEvent) {
	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(eq.ctx, payloadProcessTimeout)
	defer cancel()
	// 调用执行引擎的 NewPayload 方法，尝试插入新的执行载荷
	status, err := eq.ec.engine.NewPayload(ctx,
		ev.Envelope.ExecutionPayload, ev.Envelope.ParentBeaconBlockRoot)
	if err != nil {
		// 如果插入过程中出现错误，发出临时错误事件
		eq.emitter.Emit(rollup.EngineTemporaryErrorEvent{
			Err: fmt.Errorf("failed to insert execution payload: %w", err)})
		return
	}
	// 根据插入结果的状态进行不同的处理
	switch status.Status {
	case eth.ExecutionInvalid, eth.ExecutionInvalidBlockHash:
		// 如果执行载荷无效，发出载荷无效事件
		eq.emitter.Emit(PayloadInvalidEvent{
			Envelope: ev.Envelope,
			Err:      eth.NewPayloadErr(ev.Envelope.ExecutionPayload, status)})
		return
	case eth.ExecutionValid:
		// 如果执行载荷有效，发出载荷成功事件
		eq.emitter.Emit(PayloadSuccessEvent(ev))
		return
	default:
		// 对于其他状态，发出临时错误事件
		eq.emitter.Emit(rollup.EngineTemporaryErrorEvent{
			Err: eth.NewPayloadErr(ev.Envelope.ExecutionPayload, status)})
		return
	}
}
