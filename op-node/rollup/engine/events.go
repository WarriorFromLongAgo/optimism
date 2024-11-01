package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/log"

	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum-optimism/optimism/op-node/rollup/event"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

type Metrics interface {
	CountSequencedTxs(count int)

	RecordSequencerBuildingDiffTime(duration time.Duration)
	RecordSequencerSealingTime(duration time.Duration)
}

// ForkchoiceRequestEvent signals to the engine that it should emit an artificial
// forkchoice-update event, to signal the latest forkchoice to other derivers.
// This helps decouple derivers from the actual engine state,
// while also not making the derivers wait for a forkchoice update at random.
type ForkchoiceRequestEvent struct {
}

func (ev ForkchoiceRequestEvent) String() string {
	return "forkchoice-request"
}

// ForkchoiceUpdateEvent 是一个关键的事件，用于更新区块链的分叉选择状态。
type ForkchoiceUpdateEvent struct {
	UnsafeL2Head, SafeL2Head, FinalizedL2Head eth.L2BlockRef
}

func (ev ForkchoiceUpdateEvent) String() string {
	return "forkchoice-update"
}

// PromoteUnsafeEvent 发出信号，表示给定的块现在可能成为规范的不安全块。
// 这是 forkchoice 前的更新；更改可能尚未反映在 EL 中。
// 请注意，旧的事件前重构代码路径（处理 P2P 块）会触发此事件，
// 但手动与较新的事件处理代码路径重复。
// 参见 EngineController.InsertUnsafePayload。
// PromoteUnsafeEvent signals that the given block may now become a canonical unsafe block.
// This is pre-forkchoice update; the change may not be reflected yet in the EL.
// Note that the legacy pre-event-refactor code-path (processing P2P blocks) does fire this,
// but manually, duplicate with the newer events processing code-path.
// See EngineController.InsertUnsafePayload.
type PromoteUnsafeEvent struct {
	Ref eth.L2BlockRef
}

func (ev PromoteUnsafeEvent) String() string {
	return "promote-unsafe"
}

// RequestCrossUnsafeEvent signals that a CrossUnsafeUpdateEvent is needed.
type RequestCrossUnsafeEvent struct{}

func (ev RequestCrossUnsafeEvent) String() string {
	return "request-cross-unsafe"
}

// UnsafeUpdateEvent signals that the given block is now considered safe.
// This is pre-forkchoice update; the change may not be reflected yet in the EL.
// UnsafeUpdateEvent 表示给定的块现在被认为是安全的。
// 这是 forkchoice 之前的更新；更改可能尚未反映在 EL 中。
type UnsafeUpdateEvent struct {
	Ref eth.L2BlockRef
}

func (ev UnsafeUpdateEvent) String() string {
	return "unsafe-update"
}

// PromoteCrossUnsafeEvent signals that the given block may be promoted to cross-unsafe.
type PromoteCrossUnsafeEvent struct {
	Ref eth.L2BlockRef
}

func (ev PromoteCrossUnsafeEvent) String() string {
	return "promote-cross-unsafe"
}

// CrossUnsafeUpdateEvent signals that the given block is now considered cross-unsafe.
type CrossUnsafeUpdateEvent struct {
	CrossUnsafe eth.L2BlockRef
	LocalUnsafe eth.L2BlockRef
}

func (ev CrossUnsafeUpdateEvent) String() string {
	return "cross-unsafe-update"
}

type PendingSafeUpdateEvent struct {
	// 新的待定安全区块引用
	PendingSafe eth.L2BlockRef
	// 当前不安全头部引用
	Unsafe eth.L2BlockRef // tip, added to the signal, to determine if there are existing blocks to consolidate
}

func (ev PendingSafeUpdateEvent) String() string {
	return "pending-safe-update"
}

// PromotePendingSafeEvent signals that a block can be marked as pending-safe, and/or safe.
type PromotePendingSafeEvent struct {
	Ref         eth.L2BlockRef
	Safe        bool
	DerivedFrom eth.L1BlockRef
}

func (ev PromotePendingSafeEvent) String() string {
	return "promote-pending-safe"
}

// PromoteLocalSafeEvent signals that a block can be promoted to local-safe.
type PromoteLocalSafeEvent struct {
	Ref         eth.L2BlockRef
	DerivedFrom eth.L1BlockRef
}

func (ev PromoteLocalSafeEvent) String() string {
	return "promote-local-safe"
}

// RequestCrossSafeEvent signals that a CrossSafeUpdate is needed.
type RequestCrossSafeEvent struct{}

func (ev RequestCrossSafeEvent) String() string {
	return "request-cross-safe-update"
}

type CrossSafeUpdateEvent struct {
	CrossSafe eth.L2BlockRef
	LocalSafe eth.L2BlockRef
}

func (ev CrossSafeUpdateEvent) String() string {
	return "cross-safe-update"
}

// LocalSafeUpdateEvent signals that a block is now considered to be local-safe.
type LocalSafeUpdateEvent struct {
	Ref         eth.L2BlockRef
	DerivedFrom eth.L1BlockRef
}

func (ev LocalSafeUpdateEvent) String() string {
	return "local-safe-update"
}

// PromoteSafeEvent signals that a block can be promoted to cross-safe.
type PromoteSafeEvent struct {
	Ref         eth.L2BlockRef
	DerivedFrom eth.L1BlockRef
}

func (ev PromoteSafeEvent) String() string {
	return "promote-safe"
}

// SafeDerivedEvent signals that a block was determined to be safe, and derived from the given L1 block.
// This is signaled upon successful processing of PromoteSafeEvent.
type SafeDerivedEvent struct {
	Safe        eth.L2BlockRef
	DerivedFrom eth.L1BlockRef
}

func (ev SafeDerivedEvent) String() string {
	return "safe-derived"
}

// ProcessAttributesEvent signals to immediately process the attributes.
type ProcessAttributesEvent struct {
	Attributes *derive.AttributesWithParent
}

func (ev ProcessAttributesEvent) String() string {
	return "process-attributes"
}

type PendingSafeRequestEvent struct {
}

func (ev PendingSafeRequestEvent) String() string {
	return "pending-safe-request"
}

type ProcessUnsafePayloadEvent struct {
	Envelope *eth.ExecutionPayloadEnvelope
}

func (ev ProcessUnsafePayloadEvent) String() string {
	return "process-unsafe-payload"
}

type TryBackupUnsafeReorgEvent struct {
}

func (ev TryBackupUnsafeReorgEvent) String() string {
	return "try-backup-unsafe-reorg"
}

type TryUpdateEngineEvent struct {
}

func (ev TryUpdateEngineEvent) String() string {
	return "try-update-engine"
}

type ForceEngineResetEvent struct {
	Unsafe, Safe, Finalized eth.L2BlockRef
}

func (ev ForceEngineResetEvent) String() string {
	return "force-engine-reset"
}

type EngineResetConfirmedEvent struct {
	Unsafe, Safe, Finalized eth.L2BlockRef
}

func (ev EngineResetConfirmedEvent) String() string {
	return "engine-reset-confirmed"
}

// PromoteFinalizedEvent signals that a block can be marked as finalized.
// PromoteFinalizedEvent 表示一个块可以被标记为最终确定。
type PromoteFinalizedEvent struct {
	Ref eth.L2BlockRef
}

func (ev PromoteFinalizedEvent) String() string {
	return "promote-finalized"
}

// CrossUpdateRequestEvent triggers update events to be emitted, repeating the current state.
type CrossUpdateRequestEvent struct {
	CrossUnsafe bool
	CrossSafe   bool
}

func (ev CrossUpdateRequestEvent) String() string {
	return "cross-update-request"
}

type EngDeriver struct {
	metrics Metrics

	log     log.Logger
	cfg     *rollup.Config
	ec      *EngineController
	ctx     context.Context
	emitter event.Emitter
}

var _ event.Deriver = (*EngDeriver)(nil)

func NewEngDeriver(log log.Logger, ctx context.Context, cfg *rollup.Config,
	metrics Metrics, ec *EngineController) *EngDeriver {
	return &EngDeriver{
		log:     log,
		cfg:     cfg,
		ec:      ec,
		ctx:     ctx,
		metrics: metrics,
	}
}

func (d *EngDeriver) AttachEmitter(em event.Emitter) {
	d.emitter = em
}

func (d *EngDeriver) OnEvent(ev event.Event) bool {
	switch x := ev.(type) {
	case TryBackupUnsafeReorgEvent:
		// 如果我们不需要调用 FCU 来使用 backupUnsafe 恢复 unsafeHead，请继续，因为
		// 这是一个无操作（除了在 backupUnsafe 为空但调用 TryBackupUnsafeReorg 时纠正无效状态）。
		// If we don't need to call FCU to restore unsafeHead using backupUnsafe, keep going b/c
		// this was a no-op(except correcting invalid state when backupUnsafe is empty but TryBackupUnsafeReorg called).
		fcuCalled, err := d.ec.TryBackupUnsafeReorg(d.ctx)
		// Dealing with legacy here: it used to skip over the error-handling if fcuCalled was false.
		// But that combination is not actually a code-path in TryBackupUnsafeReorg.
		// We should drop fcuCalled, and make the function emit events directly,
		// once there are no more synchronous callers.
		// 此处处理遗留问题：如果 fcuCalled 为 false，则过去会跳过错误处理。
		// 但该组合实际上不是 TryBackupUnsafeReorg 中的代码路径。
		// 一旦不再有同步调用者，我们就应该删除 fcuCalled，并让函数直接发出事件。
		if !fcuCalled && err != nil {
			d.log.Crit("unexpected TryBackupUnsafeReorg error after no FCU call", "err", err)
		}
		if err != nil {
			// If we needed to perform a network call, then we should yield even if we did not encounter an error.
			if errors.Is(err, derive.ErrReset) {
				d.emitter.Emit(rollup.ResetEvent{Err: err})
			} else if errors.Is(err, derive.ErrTemporary) {
				d.emitter.Emit(rollup.EngineTemporaryErrorEvent{Err: err})
			} else {
				d.emitter.Emit(rollup.CriticalErrorEvent{Err: fmt.Errorf("unexpected TryBackupUnsafeReorg error type: %w", err)})
			}
		}
	case TryUpdateEngineEvent:
		// If we don't need to call FCU, keep going b/c this was a no-op. If we needed to
		// perform a network call, then we should yield even if we did not encounter an error.
		// 如果我们不需要调用 FCU，请继续，因为这是一个无操作。如果我们需要
		// 执行网络调用，那么即使没有遇到错误，我们也应该放弃。
		if err := d.ec.TryUpdateEngine(d.ctx); err != nil && !errors.Is(err, ErrNoFCUNeeded) {
			if errors.Is(err, derive.ErrReset) {
				d.emitter.Emit(rollup.ResetEvent{Err: err})
			} else if errors.Is(err, derive.ErrTemporary) {
				d.emitter.Emit(rollup.EngineTemporaryErrorEvent{Err: err})
			} else {
				d.emitter.Emit(rollup.CriticalErrorEvent{Err: fmt.Errorf("unexpected TryUpdateEngine error type: %w", err)})
			}
		}
	case ProcessUnsafePayloadEvent:
		ref, err := derive.PayloadToBlockRef(d.cfg, x.Envelope.ExecutionPayload)
		if err != nil {
			d.log.Error("failed to decode L2 block ref from payload", "err", err)
			return true
		}
		if err := d.ec.InsertUnsafePayload(d.ctx, x.Envelope, ref); err != nil {
			d.log.Info("failed to insert payload", "ref", ref,
				"txs", len(x.Envelope.ExecutionPayload.Transactions), "err", err)
			// yes, duplicate error-handling. After all derivers are interacting with the engine
			// through events, we can drop the engine-controller interface:
			// unify the events handler with the engine-controller,
			// remove a lot of code, and not do this error translation.
			if errors.Is(err, derive.ErrReset) {
				d.emitter.Emit(rollup.ResetEvent{Err: err})
			} else if errors.Is(err, derive.ErrTemporary) {
				d.emitter.Emit(rollup.EngineTemporaryErrorEvent{Err: err})
			} else {
				d.emitter.Emit(rollup.CriticalErrorEvent{Err: fmt.Errorf("unexpected InsertUnsafePayload error type: %w", err)})
			}
		} else {
			d.log.Info("successfully processed payload", "ref", ref, "txs", len(x.Envelope.ExecutionPayload.Transactions))
		}
	case ForkchoiceRequestEvent:
		d.emitter.Emit(ForkchoiceUpdateEvent{
			UnsafeL2Head:    d.ec.UnsafeL2Head(),
			SafeL2Head:      d.ec.SafeL2Head(),
			FinalizedL2Head: d.ec.Finalized(),
		})
	case ForceEngineResetEvent:
		ForceEngineReset(d.ec, x)

		// Time to apply the changes to the underlying engine
		d.emitter.Emit(TryUpdateEngineEvent{})

		log.Debug("Reset of Engine is completed",
			"safeHead", x.Safe, "unsafe", x.Unsafe, "safe_timestamp", x.Safe.Time,
			"unsafe_timestamp", x.Unsafe.Time)
		d.emitter.Emit(EngineResetConfirmedEvent(x))
	case PromoteUnsafeEvent:
		// Backup unsafeHead when new block is not built on original unsafe head.
		// 当新块没有建立在原始不安全头上时，备份 unsafeHead。
		if d.ec.unsafeHead.Number >= x.Ref.Number {
			d.ec.SetBackupUnsafeL2Head(d.ec.unsafeHead, false)
		}
		d.ec.SetUnsafeHead(x.Ref)
		d.emitter.Emit(UnsafeUpdateEvent(x))
	case UnsafeUpdateEvent:
		// pre-interop everything that is local-unsafe is also immediately cross-unsafe.
		if !d.cfg.IsInterop(x.Ref.Time) {
			d.emitter.Emit(PromoteCrossUnsafeEvent(x))
		}
		// Try to apply the forkchoice changes
		d.emitter.Emit(TryUpdateEngineEvent{})
	case PromoteCrossUnsafeEvent:
		d.ec.SetCrossUnsafeHead(x.Ref)
		d.emitter.Emit(CrossUnsafeUpdateEvent{
			CrossUnsafe: x.Ref,
			LocalUnsafe: d.ec.UnsafeL2Head(),
		})
	case RequestCrossUnsafeEvent:
		d.emitter.Emit(CrossUnsafeUpdateEvent{
			CrossUnsafe: d.ec.CrossUnsafeL2Head(),
			LocalUnsafe: d.ec.UnsafeL2Head(),
		})
	case RequestCrossSafeEvent:
		d.emitter.Emit(CrossSafeUpdateEvent{
			CrossSafe: d.ec.SafeL2Head(),
			LocalSafe: d.ec.LocalSafeL2Head(),
		})
	case PendingSafeRequestEvent:
		d.emitter.Emit(PendingSafeUpdateEvent{
			PendingSafe: d.ec.PendingSafeL2Head(),
			Unsafe:      d.ec.UnsafeL2Head(),
		})
	case PromotePendingSafeEvent:
		// Only promote if not already stale.
		// Resets/overwrites happen through engine-resets, not through promotion.
		// 仅当尚未过时才进行提升。
		// 重置/覆盖通过引擎重置进行，而不是通过提升进行。
		if x.Ref.Number > d.ec.PendingSafeL2Head().Number {
			d.ec.SetPendingSafeL2Head(x.Ref)
			d.emitter.Emit(PendingSafeUpdateEvent{
				PendingSafe: d.ec.PendingSafeL2Head(),
				Unsafe:      d.ec.UnsafeL2Head(),
			})
		}
		if x.Safe && x.Ref.Number > d.ec.LocalSafeL2Head().Number {
			d.emitter.Emit(PromoteLocalSafeEvent{
				Ref:         x.Ref,
				DerivedFrom: x.DerivedFrom,
			})
		}
	case PromoteLocalSafeEvent:
		// 设置本地安全头
		d.ec.SetLocalSafeHead(x.Ref)
		// 发出本地安全更新事件
		d.emitter.Emit(LocalSafeUpdateEvent(x))
	case LocalSafeUpdateEvent:
		// pre-interop everything that is local-safe is also immediately cross-safe.
		// 互操作前，所有本地安全的事物也立即是交叉安全的。
		if !d.cfg.IsInterop(x.Ref.Time) {
			d.emitter.Emit(PromoteSafeEvent(x))
		}
	case PromoteSafeEvent:
		d.ec.SetSafeHead(x.Ref)
		// Finalizer can pick up this safe cross-block now
		// Finalizer 现在可以拾取这个安全的跨块
		d.emitter.Emit(SafeDerivedEvent{Safe: x.Ref, DerivedFrom: x.DerivedFrom})
		d.emitter.Emit(CrossSafeUpdateEvent{
			CrossSafe: d.ec.SafeL2Head(),
			LocalSafe: d.ec.LocalSafeL2Head(),
		})
		// Try to apply the forkchoice changes
		// 尝试应用 forkchoice 更改
		d.emitter.Emit(TryUpdateEngineEvent{})
	case PromoteFinalizedEvent:
		// 检查是否试图回滚已确认的区块（不允许）
		if x.Ref.Number < d.ec.Finalized().Number {
			d.log.Error("Cannot rewind finality,", "ref", x.Ref, "finalized", d.ec.Finalized())
			return true
		}
		// 确保要最终确认的区块已经是安全的
		if x.Ref.Number > d.ec.SafeL2Head().Number {
			d.log.Error("Block must be safe before it can be finalized", "ref", x.Ref, "safe", d.ec.SafeL2Head())
			return true
		}
		// 设置新的最终确认区块头
		d.ec.SetFinalizedHead(x.Ref)
		// 触发引擎状态更新事件
		// Try to apply the forkchoice changes
		d.emitter.Emit(TryUpdateEngineEvent{})
	case CrossUpdateRequestEvent:
		if x.CrossUnsafe {
			d.emitter.Emit(CrossUnsafeUpdateEvent{
				CrossUnsafe: d.ec.CrossUnsafeL2Head(),
				LocalUnsafe: d.ec.UnsafeL2Head(),
			})
		}
		if x.CrossSafe {
			d.emitter.Emit(CrossSafeUpdateEvent{
				CrossSafe: d.ec.SafeL2Head(),
				LocalSafe: d.ec.LocalSafeL2Head(),
			})
		}
	case BuildStartEvent:
		d.onBuildStart(x)
	case BuildStartedEvent:
		d.onBuildStarted(x)
	case BuildSealedEvent:
		d.onBuildSealed(x)
	case BuildSealEvent:
		d.onBuildSeal(x)
	case BuildInvalidEvent:
		d.onBuildInvalid(x)
	case BuildCancelEvent:
		d.onBuildCancel(x)
	case PayloadProcessEvent:
		d.onPayloadProcess(x)
	case PayloadSuccessEvent:
		d.onPayloadSuccess(x)
	case PayloadInvalidEvent:
		d.onPayloadInvalid(x)
	default:
		return false
	}
	return true
}

type ResetEngineControl interface {
	// SetUnsafeHead (): 设置不安全头部。
	SetUnsafeHead(eth.L2BlockRef)
	// SetSafeHead (): 设置安全头部。
	SetSafeHead(eth.L2BlockRef)
	// SetFinalizedHead (): 设置最终确认头部。
	SetFinalizedHead(eth.L2BlockRef)
	// SetLocalSafeHead (): 设置本地安全头部。
	SetLocalSafeHead(ref eth.L2BlockRef)
	// SetCrossUnsafeHead (): 设置跨链不安全头部。
	SetCrossUnsafeHead(ref eth.L2BlockRef)
	// SetBackupUnsafeL2Head ():  设置备份的不安全 L2 头部。
	SetBackupUnsafeL2Head(block eth.L2BlockRef, triggerReorg bool)
	// SetPendingSafeL2Head (): 设置待定的安全 L2 头部。
	SetPendingSafeL2Head(eth.L2BlockRef)
}

// ForceEngineReset is not to be used. The op-program needs it for now, until event processing is adopted there.
func ForceEngineReset(ec ResetEngineControl, x ForceEngineResetEvent) {
	ec.SetUnsafeHead(x.Unsafe)
	ec.SetLocalSafeHead(x.Safe)
	ec.SetPendingSafeL2Head(x.Safe)
	ec.SetFinalizedHead(x.Finalized)
	ec.SetSafeHead(x.Safe)
	ec.SetCrossUnsafeHead(x.Safe)
	ec.SetBackupUnsafeL2Head(eth.L2BlockRef{}, false)
}
