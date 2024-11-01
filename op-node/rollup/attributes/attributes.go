package attributes

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/log"

	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum-optimism/optimism/op-node/rollup/engine"
	"github.com/ethereum-optimism/optimism/op-node/rollup/event"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

type L2 interface {
	PayloadByNumber(context.Context, uint64) (*eth.ExecutionPayloadEnvelope, error)
}

type AttributesHandler struct {
	log log.Logger
	cfg *rollup.Config

	// when the rollup node shuts down, stop any in-flight sub-processes of the attributes-handler
	ctx context.Context

	l2 L2

	mu sync.Mutex

	emitter event.Emitter

	attributes     *derive.AttributesWithParent
	sentAttributes bool
}

func NewAttributesHandler(log log.Logger, cfg *rollup.Config, ctx context.Context, l2 L2) *AttributesHandler {
	return &AttributesHandler{
		log:        log,
		cfg:        cfg,
		ctx:        ctx,
		l2:         l2,
		attributes: nil,
	}
}

func (eq *AttributesHandler) AttachEmitter(em event.Emitter) {
	eq.emitter = em
}

func (eq *AttributesHandler) OnEvent(ev event.Event) bool {
	// Events may be concurrent in the future. Prevent unsafe concurrent modifications to the attributes.
	eq.mu.Lock()
	defer eq.mu.Unlock()

	switch x := ev.(type) {
	case engine.PendingSafeUpdateEvent:
		eq.onPendingSafeUpdate(x)
	case derive.DerivedAttributesEvent:
		eq.attributes = x.Attributes
		eq.emitter.Emit(derive.ConfirmReceivedAttributesEvent{})
		// 确保我们有一个预状态信号来处理属性
		// to make sure we have a pre-state signal to process the attributes from
		eq.emitter.Emit(engine.PendingSafeRequestEvent{})
	case rollup.ResetEvent:
		eq.sentAttributes = false
		eq.attributes = nil
	case rollup.EngineTemporaryErrorEvent:
		eq.sentAttributes = false
	case engine.InvalidPayloadAttributesEvent:
		if x.Attributes.DerivedFrom == (eth.L1BlockRef{}) {
			return true // from sequencing
		}
		eq.sentAttributes = false
		// If the engine signals that attributes are invalid,
		// that should match our last applied attributes, which we should thus drop.
		eq.attributes = nil
		// Time to re-evaluate without attributes.
		// (the pending-safe state will then be forwarded to our source of attributes).
		eq.emitter.Emit(engine.PendingSafeRequestEvent{})
	case engine.PayloadSealExpiredErrorEvent:
		if x.DerivedFrom == (eth.L1BlockRef{}) {
			return true // from sequencing
		}
		eq.log.Warn("Block sealing job of derived attributes expired, job will be re-attempted.",
			"build_id", x.Info.ID, "timestamp", x.Info.Timestamp, "err", x.Err)
		// If the engine failed to seal temporarily, just allow to resubmit (triggered on next safe-head poke)
		eq.sentAttributes = false
	case engine.PayloadSealInvalidEvent:
		if x.DerivedFrom == (eth.L1BlockRef{}) {
			return true // from sequencing
		}
		eq.log.Warn("Cannot seal derived block attributes, input is invalid",
			"build_id", x.Info.ID, "timestamp", x.Info.Timestamp, "err", x.Err)
		eq.sentAttributes = false
		eq.attributes = nil
		eq.emitter.Emit(engine.PendingSafeRequestEvent{})
	default:
		return false
	}
	return true
}

// onPendingSafeUpdate applies the queued-up block attributes, if any, on top of the signaled pending state.
// The event is also used to clear the queued-up attributes, when successfully processed.
// On processing failure this may emit a temporary, reset, or critical error like other derivers.
// onPendingSafeUpdate 将排队的块属性（如果有）应用于已发出信号的待处理状态。
// 成功处理后，此事件还用于清除排队的属性。
// 处理失败时，这可能会像其他派生程序一样发出临时、重置或严重错误。
func (eq *AttributesHandler) onPendingSafeUpdate(x engine.PendingSafeUpdateEvent) {
	// 验证链状态：确保pending safe不会超前于unsafe区块
	if x.Unsafe.Number < x.PendingSafe.Number {
		// invalid chain state, reset to try and fix it
		// 如果链状态无效，发出重置事件
		eq.emitter.Emit(rollup.ResetEvent{Err: fmt.Errorf("pending-safe label (%d) may not be ahead of unsafe head label (%d)", x.PendingSafe.Number, x.Unsafe.Number)})
		return
	}
	// 如果没有待处理的属性，请求生成新的属性
	if eq.attributes == nil {
		eq.sentAttributes = false
		// Request new attributes to be generated, only if we don't currently have attributes that have yet to be processed.
		// It is safe to request the pipeline, the attributes-handler is the only user of it,
		// and the pipeline will not generate another set of attributes until the last set is recognized.
		// 仅当我们当前没有尚未处理的属性时，才请求生成新属性。
		// 请求管道是安全的，属性处理程序是它的唯一用户，
		// 并且管道在识别最后一组属性之前不会生成另一组属性。
		// 发出管道步骤事件，请求生成新属性
		eq.emitter.Emit(derive.PipelineStepEvent{PendingSafe: x.PendingSafe})
		return
	}

	// Drop attributes if they don't apply on top of the pending safe head.
	// This is expected after successful processing of these attributes.
	// 检查属性是否过期：如果属性的父区块号与pending safe不匹配
	if eq.attributes.Parent.Number != x.PendingSafe.Number {
		eq.log.Debug("dropping stale attributes, requesting new ones",
			"pending", x.PendingSafe, "attributes_parent", eq.attributes.Parent)
		eq.attributes = nil
		eq.sentAttributes = false
		eq.emitter.Emit(derive.PipelineStepEvent{PendingSafe: x.PendingSafe})
		return
	}
	// 避免重复发送属性
	if eq.sentAttributes {
		eq.log.Warn("already sent the existing attributes")
		return
	}

	// 处理区块重组情况：检查属性的父区块是否与pending safe匹配
	if eq.attributes.Parent != x.PendingSafe {
		// If the attributes are supposed to follow the pending safe head, but don't build on the exact block,
		// then there's some reorg inconsistency. Either bad attributes, or bad pending safe head.
		// Trigger a reset, and the system can derive attributes on top of the pending safe head.
		// Until the reset is complete we don't clear the attributes state,
		// so we can re-emit the ResetEvent until the reset actually happens.
		// 如果不匹配，发出重置事件
		eq.emitter.Emit(rollup.ResetEvent{Err: fmt.Errorf("pending safe head changed to %s with parent %s, conflicting with queued safe attributes on top of %s",
			x.PendingSafe, x.PendingSafe.ParentID(), eq.attributes.Parent)})
	} else {
		// 如果pending safe落后于unsafe，尝试合并区块
		// if there already exists a block we can just consolidate it
		if x.PendingSafe.Number < x.Unsafe.Number {
			eq.consolidateNextSafeAttributes(eq.attributes, x.PendingSafe)
		} else {
			// 否则，直接开始构建新区块
			// append to tip otherwise
			eq.sentAttributes = true
			eq.emitter.Emit(engine.BuildStartEvent{Attributes: eq.attributes})
		}
	}
}

// consolidateNextSafeAttributes tries to match the next safe attributes against the existing unsafe chain,
// to avoid extra processing or unnecessary unwinding of the chain.
// However, if the attributes do not match, they will be forced to process the attributes.
// consolidateNextSafeAttributes 尝试将下一个安全属性与现有的不安全链进行匹配，
// 以避免额外的处理或不必要的链解开。
// 但是，如果属性不匹配，它们将被强制处理这些属性。
// 主要负责将新的安全属性与现有的不安全链进行匹配和合并。
func (eq *AttributesHandler) consolidateNextSafeAttributes(attributes *derive.AttributesWithParent, onto eth.L2BlockRef) {
	// 创建一个10秒超时的上下文
	ctx, cancel := context.WithTimeout(eq.ctx, time.Second*10)
	defer cancel()

	// 通过区块号获取现有的不安全区块
	// attributes.Parent.Number+1 表示要获取的是父区块的下一个区块
	envelope, err := eq.l2.PayloadByNumber(ctx, attributes.Parent.Number+1)
	if err != nil {
		if errors.Is(err, ethereum.NotFound) {
			// 如果区块未找到，可能是引擎重启或安全头不一致，需要重置
			// engine may have restarted, or inconsistent safe head. We need to reset
			eq.emitter.Emit(rollup.ResetEvent{Err: fmt.Errorf("expected engine was synced and had unsafe block to reconcile, but cannot find the block: %w", err)})
			return
		}
		// 其他错误情况，发出临时错误事件
		eq.emitter.Emit(rollup.EngineTemporaryErrorEvent{Err: fmt.Errorf("failed to get existing unsafe payload to compare against derived attributes from L1: %w", err)})
		return
	}
	// 比较新的安全属性与现有区块是否匹配
	if err := AttributesMatchBlock(eq.cfg, attributes.Attributes, onto.Hash, envelope, eq.log); err != nil {
		// 如果不匹配，记录警告并触发区块重组
		eq.log.Warn("L2 reorg: existing unsafe block does not match derived attributes from L1",
			"err", err, "unsafe", envelope.ExecutionPayload.ID(), "pending_safe", onto)

		eq.sentAttributes = true
		// 发出构建事件，开始重组过程
		// geth cannot wind back a chain without reorging to a new, previously non-canonical, block
		eq.emitter.Emit(engine.BuildStartEvent{Attributes: attributes})
		return
	} else {
		// 如果匹配，将区块转换为区块引用
		ref, err := derive.PayloadToBlockRef(eq.cfg, envelope.ExecutionPayload)
		if err != nil {
			eq.log.Error("Failed to compute block-ref from execution payload")
			return
		}
		// 发出提升事件，将区块状态提升为pending safe
		eq.emitter.Emit(engine.PromotePendingSafeEvent{
			Ref:         ref,
			Safe:        attributes.IsLastInSpan,
			DerivedFrom: attributes.DerivedFrom,
		})
	}

	// unsafe head stays the same, we did not reorg the chain.
}
