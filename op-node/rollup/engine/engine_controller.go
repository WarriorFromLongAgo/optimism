package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"

	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum-optimism/optimism/op-node/rollup/event"
	"github.com/ethereum-optimism/optimism/op-node/rollup/sync"
	"github.com/ethereum-optimism/optimism/op-service/clock"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

type syncStatusEnum int

const (
	// CL同步主要关注共识层面的快速同步，而EL同步则处理更详细的执行层数据。
	// CL同步通常从L1以太坊网络或其他Optimism节点获取数据，而EL同步则主要从Optimism的L2网络或其他Optimism节点获取数据。

	// CL同步（Consensus Layer Synchronization）：
	// 作用：同步共识层的数据，包括区块头、验证者信息等。CL同步主要获取区块头、验证者信息等轻量级数据。
	// 运行情况：通常在节点启动时或需要快速同步最新状态时运行。
	// 主要功能：
	//	 确保节点的共识状态与网络一致
	//	 同步最新的区块头信息
	//	 处理分叉选择
	//	 跟踪网络的安全头部和最终确认状态
	// 共识层(Consensus Layer)：负责区块的排序、验证者管理和最终确定性。在Optimism中，这通常指的是与L1以太坊主网相关的共识机制。
	//

	// EL同步（Execution Layer Synchronization）：
	// 作用：同步执行层的数据，包括完整的区块内容、交易和状态。EL同步获取完整的区块内容、交易数据和状态。
	// 运行情况：在以下情况下运行：
	//	 节点首次启动且没有最终确认的区块时
	//	 节点落后且需要同步大量区块时
	// 主要功能：
	//	 同步完整的区块数据和状态
	//	 执行交易并更新本地状态
	//	 验证区块的正确性
	// 执行层(Execution Layer)：负责执行交易、维护状态和处理智能合约。在Optimism中，这指的是L2网络的实际交易执行环境。

	syncStatusCL syncStatusEnum = iota
	// We transition between the 4 EL states linearly. We spend the majority of the time in the second & fourth.
	// We only want to EL sync if there is no finalized block & once we finish EL sync we need to mark the last block
	// as finalized so we can switch to consolidation
	// TODO(protocol-quest#91): We can restart EL sync & still consolidate if there finalized blocks on the execution client if the
	// execution client is running in archive mode. In some cases we may want to switch back from CL to EL sync, but that is complicated.
	// 我们在 4 个 EL 状态之间线性转换。我们大部分时间都花在第二和第四个状态上。
	// 我们只想在没有最终确定的块的情况下进行 EL 同步，一旦我们完成 EL 同步，我们就需要将最后一个块标记为
	// 已确定，这样我们就可以切换到合并
	// TODO（协议任务#91）：如果执行客户端在存档模式下运行，我们可以重新启动 EL 同步，并且如果执行客户端上有最终确定的块，我们仍然可以合并。在某些情况下，我们可能希望从 CL 切换回 EL 同步，但这很复杂。

	// 首先，如果我们被引导到 EL 同步，请检查是否尚未完成任何操作
	syncStatusWillStartEL // First if we are directed to EL sync, check that nothing has been finalized yet
	// 执行 EL 同步
	syncStatusStartedEL // Perform our EL sync
	// EL 同步已完成，但我们需要将最终同步块标记为已完成
	syncStatusFinishedELButNotFinalized // EL sync is done, but we need to mark the final sync block as finalized
	// EL 同步已完成，我们应该进行合并
	syncStatusFinishedEL // EL sync is done & we should be performing consolidation
)

var ErrNoFCUNeeded = errors.New("no FCU call was needed")

type ExecEngine interface {
	GetPayload(ctx context.Context, payloadInfo eth.PayloadInfo) (*eth.ExecutionPayloadEnvelope, error)
	ForkchoiceUpdate(ctx context.Context, state *eth.ForkchoiceState, attr *eth.PayloadAttributes) (*eth.ForkchoiceUpdatedResult, error)
	NewPayload(ctx context.Context, payload *eth.ExecutionPayload, parentBeaconBlockRoot *common.Hash) (*eth.PayloadStatusV1, error)
	L2BlockRefByLabel(ctx context.Context, label eth.BlockLabel) (eth.L2BlockRef, error)
}

type EngineController struct {
	engine     ExecEngine // Underlying execution engine RPC
	log        log.Logger
	metrics    derive.Metrics
	syncCfg    *sync.Config
	syncStatus syncStatusEnum
	chainSpec  *rollup.ChainSpec
	rollupCfg  *rollup.Config
	elStart    time.Time
	clock      clock.Clock

	emitter event.Emitter

	// Block Head State
	unsafeHead eth.L2BlockRef
	// Cross-verified unsafeHead, always equal to unsafeHead pre-interop
	crossUnsafeHead eth.L2BlockRef
	// Pending localSafeHead
	// L2 block processed from the middle of a span batch,
	// but not marked as the safe block yet.
	pendingSafeHead eth.L2BlockRef
	// Derived from L1, and known to be a completed span-batch,
	// but not cross-verified yet.
	localSafeHead eth.L2BlockRef
	// Derived from L1 and cross-verified to have cross-safe dependencies.
	safeHead eth.L2BlockRef
	// Derived from finalized L1 data,
	// and cross-verified to only have finalized dependencies.
	finalizedHead eth.L2BlockRef
	// The unsafe head to roll back to,
	// after the pendingSafeHead fails to become safe.
	// This is changing in the Holocene fork.
	backupUnsafeHead eth.L2BlockRef

	needFCUCall bool
	// Track when the rollup node changes the forkchoice to restore previous
	// known unsafe chain. e.g. Unsafe Reorg caused by Invalid span batch.
	// This update does not retry except engine returns non-input error
	// because engine may forgot backupUnsafeHead or backupUnsafeHead is not part
	// of the chain.
	needFCUCallForBackupUnsafeReorg bool
}

func NewEngineController(engine ExecEngine, log log.Logger, metrics derive.Metrics,
	rollupCfg *rollup.Config, syncCfg *sync.Config, emitter event.Emitter) *EngineController {
	syncStatus := syncStatusCL
	if syncCfg.SyncMode == sync.ELSync {
		syncStatus = syncStatusWillStartEL
	}

	return &EngineController{
		engine:     engine,
		log:        log,
		metrics:    metrics,
		chainSpec:  rollup.NewChainSpec(rollupCfg),
		rollupCfg:  rollupCfg,
		syncCfg:    syncCfg,
		syncStatus: syncStatus,
		clock:      clock.SystemClock,
		emitter:    emitter,
	}
}

// State Getters

func (e *EngineController) UnsafeL2Head() eth.L2BlockRef {
	return e.unsafeHead
}

func (e *EngineController) CrossUnsafeL2Head() eth.L2BlockRef {
	return e.crossUnsafeHead
}

func (e *EngineController) PendingSafeL2Head() eth.L2BlockRef {
	return e.pendingSafeHead
}

func (e *EngineController) LocalSafeL2Head() eth.L2BlockRef {
	return e.localSafeHead
}

func (e *EngineController) SafeL2Head() eth.L2BlockRef {
	return e.safeHead
}

func (e *EngineController) Finalized() eth.L2BlockRef {
	return e.finalizedHead
}

func (e *EngineController) BackupUnsafeL2Head() eth.L2BlockRef {
	return e.backupUnsafeHead
}

func (e *EngineController) IsEngineSyncing() bool {
	return e.syncStatus == syncStatusWillStartEL || e.syncStatus == syncStatusStartedEL || e.syncStatus == syncStatusFinishedELButNotFinalized
}

// Setters

// SetFinalizedHead implements LocalEngineControl.
func (e *EngineController) SetFinalizedHead(r eth.L2BlockRef) {
	e.metrics.RecordL2Ref("l2_finalized", r)
	e.finalizedHead = r
	e.needFCUCall = true
}

// SetPendingSafeL2Head implements LocalEngineControl.
func (e *EngineController) SetPendingSafeL2Head(r eth.L2BlockRef) {
	e.metrics.RecordL2Ref("l2_pending_safe", r)
	e.pendingSafeHead = r
}

// SetLocalSafeHead sets the local-safe head.
func (e *EngineController) SetLocalSafeHead(r eth.L2BlockRef) {
	e.metrics.RecordL2Ref("l2_local_safe", r)
	e.localSafeHead = r
}

// SetSafeHead sets the cross-safe head.
func (e *EngineController) SetSafeHead(r eth.L2BlockRef) {
	e.metrics.RecordL2Ref("l2_safe", r)
	e.safeHead = r
	e.needFCUCall = true
}

// SetUnsafeHead sets the local-unsafe head.
// 更新 L2 链的不安全头部状态，并为后续的状态更新和分叉检查做准备。
func (e *EngineController) SetUnsafeHead(r eth.L2BlockRef) {
	// 记录新的不安全 L2 头部引用的指标：
	e.metrics.RecordL2Ref("l2_unsafe", r)
	// 将传入的 L2 区块引用 r 赋值给 e.unsafeHead。
	e.unsafeHead = r
	// 标记需要进行分叉选择更新：
	// 设置 e.needFCUCall = true，表示需要在之后进行分叉选择更新（FCU: Fork Choice Update）。
	e.needFCUCall = true
	// 检查分叉激活：来检查是否有新的分叉被激活。
	e.chainSpec.CheckForkActivation(e.log, r)
}

// SetCrossUnsafeHead the cross-unsafe head.
func (e *EngineController) SetCrossUnsafeHead(r eth.L2BlockRef) {
	e.metrics.RecordL2Ref("l2_cross_unsafe", r)
	e.crossUnsafeHead = r
}

// SetBackupUnsafeL2Head implements LocalEngineControl.
func (e *EngineController) SetBackupUnsafeL2Head(r eth.L2BlockRef, triggerReorg bool) {
	e.metrics.RecordL2Ref("l2_backup_unsafe", r)
	e.backupUnsafeHead = r
	e.needFCUCallForBackupUnsafeReorg = triggerReorg
}

// logSyncProgressMaybe helps log forkchoice state-changes when applicable.
// First, the pre-state is registered.
// A callback is returned to then log the changes to the pre-state, if any.
// logSyncProgressMaybe 在适用时帮助记录 forkchoice 状态变化。
// 首先，注册预状态。
// 然后返回一个回调，以记录对预状态的更改（如果有）。
func (e *EngineController) logSyncProgressMaybe() func() {
	prevFinalized := e.finalizedHead
	prevSafe := e.safeHead
	prevPendingSafe := e.pendingSafeHead
	prevUnsafe := e.unsafeHead
	prevBackupUnsafe := e.backupUnsafeHead
	return func() {
		// if forkchoice still needs to be updated, then the last change was unsuccessful, thus no progress to log.
		if e.needFCUCall || e.needFCUCallForBackupUnsafeReorg {
			return
		}
		var reason string
		if prevFinalized != e.finalizedHead {
			reason = "finalized block"
		} else if prevSafe != e.safeHead {
			if prevSafe == prevUnsafe {
				reason = "derived safe block from L1"
			} else {
				reason = "consolidated block with L1"
			}
		} else if prevUnsafe != e.unsafeHead {
			reason = "new chain head block"
		} else if prevPendingSafe != e.pendingSafeHead {
			reason = "pending new safe block"
		} else if prevBackupUnsafe != e.backupUnsafeHead {
			reason = "new backup unsafe block"
		}
		if reason != "" {
			e.log.Info("Sync progress",
				"reason", reason,
				"l2_finalized", e.finalizedHead,
				"l2_safe", e.safeHead,
				"l2_pending_safe", e.pendingSafeHead,
				"l2_unsafe", e.unsafeHead,
				"l2_backup_unsafe", e.backupUnsafeHead,
				"l2_time", e.UnsafeL2Head().Time,
			)
		}
	}
}

// Misc Setters only used by the engine queue

// checkNewPayloadStatus checks returned status of engine_newPayloadV1 request for next unsafe payload.
// It returns true if the status is acceptable.
// checkNewPayloadStatus 检查 engine_newPayloadV1 请求返回的下一个不安全有效负载的状态。
// 如果状态可以接受，则返回 true。
func (e *EngineController) checkNewPayloadStatus(status eth.ExecutePayloadStatus) bool {
	if e.syncCfg.SyncMode == sync.ELSync {
		if status == eth.ExecutionValid && e.syncStatus == syncStatusStartedEL {
			e.syncStatus = syncStatusFinishedELButNotFinalized
		}
		// Allow SYNCING and ACCEPTED if engine EL sync is enabled
		return status == eth.ExecutionValid || status == eth.ExecutionSyncing || status == eth.ExecutionAccepted
	}
	return status == eth.ExecutionValid
}

// checkForkchoiceUpdatedStatus checks returned status of engine_forkchoiceUpdatedV1 request for next unsafe payload.
// It returns true if the status is acceptable.
func (e *EngineController) checkForkchoiceUpdatedStatus(status eth.ExecutePayloadStatus) bool {
	if e.syncCfg.SyncMode == sync.ELSync {
		if status == eth.ExecutionValid && e.syncStatus == syncStatusStartedEL {
			e.syncStatus = syncStatusFinishedELButNotFinalized
		}
		// Allow SYNCING if engine P2P sync is enabled
		return status == eth.ExecutionValid || status == eth.ExecutionSyncing
	}
	return status == eth.ExecutionValid
}

// TryUpdateEngine attempts to update the engine with the current forkchoice state of the rollup node,
// this is a no-op if the nodes already agree on the forkchoice state.
// TryUpdateEngine 尝试使用汇总节点的当前 forkchoice 状态更新引擎，
// 如果节点已经就 forkchoice 状态达成一致，则这是一个无操作。
// 用于常规的引擎状态更新，处理正常的分叉选择更新，可以更新所有头部（不安全、安全、最终确认），维护整个链的状态一致性
func (e *EngineController) TryUpdateEngine(ctx context.Context) error {
	// 检查是否需要执行分叉选择更新
	if !e.needFCUCall {
		return ErrNoFCUNeeded
	}
	// 如果引擎正在同步，记录警告日志
	if e.IsEngineSyncing() {
		e.log.Warn("Attempting to update forkchoice state while EL syncing")
	}
	// 验证不安全头部不能在已确认头部之前
	if e.unsafeHead.Number < e.finalizedHead.Number {
		err := fmt.Errorf("invalid forkchoice state, unsafe head %s is behind finalized head %s", e.unsafeHead, e.finalizedHead)
		// 发出严重错误事件，使节点退出
		e.emitter.Emit(rollup.CriticalErrorEvent{Err: err}) // make the node exit, things are very wrong.
		return err
	}
	// 准备分叉选择状态
	fc := eth.ForkchoiceState{
		HeadBlockHash:      e.unsafeHead.Hash,
		SafeBlockHash:      e.safeHead.Hash,
		FinalizedBlockHash: e.finalizedHead.Hash,
	}
	// 记录同步进度
	logFn := e.logSyncProgressMaybe()
	defer logFn()
	// 执行分叉选择更新
	fcRes, err := e.engine.ForkchoiceUpdate(ctx, &fc, nil)
	if err != nil {
		var inputErr eth.InputError
		if errors.As(err, &inputErr) {
			switch inputErr.Code {
			case eth.InvalidForkchoiceState:
				return derive.NewResetError(fmt.Errorf("forkchoice update was inconsistent with engine, need reset to resolve: %w", inputErr.Unwrap()))
			default:
				return derive.NewTemporaryError(fmt.Errorf("unexpected error code in forkchoice-updated response: %w", err))
			}
		} else {
			return derive.NewTemporaryError(fmt.Errorf("failed to sync forkchoice with engine: %w", err))
		}
	}
	// 如果更新成功，发出事件通知
	if fcRes.PayloadStatus.Status == eth.ExecutionValid {
		e.emitter.Emit(ForkchoiceUpdateEvent{
			UnsafeL2Head:    e.unsafeHead,
			SafeL2Head:      e.safeHead,
			FinalizedL2Head: e.finalizedHead,
		})
	}
	// 如果所有头部都一致，清除备份的不安全头部
	if e.unsafeHead == e.safeHead && e.safeHead == e.pendingSafeHead {
		// Remove backupUnsafeHead because this backup will be never used after consolidation.
		e.SetBackupUnsafeL2Head(eth.L2BlockRef{}, false)
	}
	// 重置更新标志
	e.needFCUCall = false
	return nil
}

// InsertUnsafePayload 主要功能是向执行引擎插入一个新的不安全执行载荷（通常是一个新的 L2 区块），并更新相关的状态。这个方法是 Optimism 网络中处理新区块的关键部分。
func (e *EngineController) InsertUnsafePayload(ctx context.Context, envelope *eth.ExecutionPayloadEnvelope, ref eth.L2BlockRef) error {
	// Check if there is a finalized head once when doing EL sync. If so, transition to CL sync
	// 检查同步状态，如果需要，开始或跳过 EL 同步
	if e.syncStatus == syncStatusWillStartEL {
		b, err := e.engine.L2BlockRefByLabel(ctx, eth.Finalized)
		rollupGenesisIsFinalized := b.Hash == e.rollupCfg.Genesis.L2.Hash
		// 根据不同情况决定是否开始 EL 同步
		if errors.Is(err, ethereum.NotFound) || rollupGenesisIsFinalized || e.syncCfg.SupportsPostFinalizationELSync {
			// 开始 EL 同步
			e.syncStatus = syncStatusStartedEL
			e.log.Info("Starting EL sync")
			e.elStart = e.clock.Now()
		} else if err == nil {
			// 跳过 EL 同步，直接进行 CL 同步
			e.syncStatus = syncStatusFinishedEL
			e.log.Info("Skipping EL sync and going straight to CL sync because there is a finalized block", "id", b.ID())
			return nil
		} else {
			// 获取最终确认头失败，返回临时错误
			return derive.NewTemporaryError(fmt.Errorf("failed to fetch finalized head: %w", err))
		}
	}
	// Insert the payload & then call FCU
	// 向执行引擎插入新的载荷，接收一个新的执行载荷（区块）
	status, err := e.engine.NewPayload(ctx, envelope.ExecutionPayload, envelope.ParentBeaconBlockRoot)
	if err != nil {
		return derive.NewTemporaryError(fmt.Errorf("failed to update insert payload: %w", err))
	}
	// 检查插入结果，如果无效则发出事件通知
	if status.Status == eth.ExecutionInvalid {
		e.emitter.Emit(PayloadInvalidEvent{Envelope: envelope, Err: eth.NewPayloadErr(envelope.ExecutionPayload, status)})
	}
	if !e.checkNewPayloadStatus(status.Status) {
		payload := envelope.ExecutionPayload
		return derive.NewTemporaryError(fmt.Errorf("cannot process unsafe payload: new - %v; parent: %v; err: %w",
			payload.ID(), payload.ParentID(), eth.NewPayloadErr(payload, status)))
	}

	// Mark the new payload as valid
	// 准备更新分叉选择状态
	fc := eth.ForkchoiceState{
		HeadBlockHash:      envelope.ExecutionPayload.BlockHash,
		SafeBlockHash:      e.safeHead.Hash,
		FinalizedBlockHash: e.finalizedHead.Hash,
	}
	// 如果 EL 同步刚完成但未最终确认，更新相关状态
	if e.syncStatus == syncStatusFinishedELButNotFinalized {
		// ... (更新安全头和最终确认头)
		fc.SafeBlockHash = envelope.ExecutionPayload.BlockHash
		fc.FinalizedBlockHash = envelope.ExecutionPayload.BlockHash
		e.SetUnsafeHead(ref) // ensure that the unsafe head stays ahead of safe/finalized labels.
		e.emitter.Emit(UnsafeUpdateEvent{Ref: ref})
		e.SetLocalSafeHead(ref)
		e.SetSafeHead(ref)
		e.emitter.Emit(CrossSafeUpdateEvent{LocalSafe: ref, CrossSafe: ref})
		e.SetFinalizedHead(ref)
	}
	// 记录同步进度
	logFn := e.logSyncProgressMaybe()
	defer logFn()
	// 向执行引擎发送分叉选择更新
	// 通过 ForkchoiceUpdate 将新区块设置为规范链的一部分
	// 确保新区块成为活跃链的一部分，而不是作为分叉存在
	fcRes, err := e.engine.ForkchoiceUpdate(ctx, &fc, nil)
	if err != nil {
		// 处理各种错误情况
		var inputErr eth.InputError
		if errors.As(err, &inputErr) {
			switch inputErr.Code {
			case eth.InvalidForkchoiceState:
				return derive.NewResetError(fmt.Errorf("pre-unsafe-block forkchoice update was inconsistent with engine, need reset to resolve: %w", inputErr.Unwrap()))
			default:
				return derive.NewTemporaryError(fmt.Errorf("unexpected error code in forkchoice-updated response: %w", err))
			}
		} else {
			return derive.NewTemporaryError(fmt.Errorf("failed to update forkchoice to prepare for new unsafe payload: %w", err))
		}
	}
	// 处理更新结果，如果出错则返回相应的错误
	if !e.checkForkchoiceUpdatedStatus(fcRes.PayloadStatus.Status) {
		payload := envelope.ExecutionPayload
		return derive.NewTemporaryError(fmt.Errorf("cannot prepare unsafe chain for new payload: new - %v; parent: %v; err: %w",
			payload.ID(), payload.ParentID(), eth.ForkchoiceUpdateErr(fcRes.PayloadStatus)))
	}
	// 设置新的不安全头部
	e.SetUnsafeHead(ref)
	e.needFCUCall = false
	e.emitter.Emit(UnsafeUpdateEvent{Ref: ref})
	// 如果 EL 同步刚完成，记录日志并更新状态
	if e.syncStatus == syncStatusFinishedELButNotFinalized {
		// ... (记录同步完成日志，更新同步状态)
		e.log.Info("Finished EL sync", "sync_duration", e.clock.Since(e.elStart), "finalized_block", ref.ID().String())
		e.syncStatus = syncStatusFinishedEL
	}
	// 如果分叉选择更新有效，发出事件通知
	if fcRes.PayloadStatus.Status == eth.ExecutionValid {
		e.emitter.Emit(ForkchoiceUpdateEvent{
			UnsafeL2Head:    e.unsafeHead,
			SafeL2Head:      e.safeHead,
			FinalizedL2Head: e.finalizedHead,
		})
	}

	return nil
}

// shouldTryBackupUnsafeReorg 检查是否需要将不安全头重组（恢复）到 backupUnsafeHead。
// 返回决定触发 FCU 的布尔值。
// shouldTryBackupUnsafeReorg checks reorging(restoring) unsafe head to backupUnsafeHead is needed.
// Returns boolean which decides to trigger FCU.
func (e *EngineController) shouldTryBackupUnsafeReorg() bool {
	if !e.needFCUCallForBackupUnsafeReorg {
		return false
	}
	// This method must be never called when EL sync. If EL sync is in progress, early return.
	if e.IsEngineSyncing() {
		e.log.Warn("Attempting to unsafe reorg using backupUnsafe while EL syncing")
		return false
	}
	if e.BackupUnsafeL2Head() == (eth.L2BlockRef{}) { // sanity check backupUnsafeHead is there
		e.log.Warn("Attempting to unsafe reorg using backupUnsafe even though it is empty")
		e.SetBackupUnsafeL2Head(eth.L2BlockRef{}, false)
		return false
	}
	return true
}

// TryBackupUnsafeReorg 尝试将不安全头重组（恢复）到 backupUnsafeHead。
// 如果成功，则将当前 forkchoice 状态更新到 rollup 节点。
// TryBackupUnsafeReorg attempts to reorg(restore) unsafe head to backupUnsafeHead.
// If succeeds, update current forkchoice state to the rollup node.
// 主要用于处理不安全头部的回滚操作，只在需要恢复到备份的不安全头部时使用，不会更新安全头部和最终确认头部，成功后会将当前不安全头部设置为备份的不安全头部
func (e *EngineController) TryBackupUnsafeReorg(ctx context.Context) (bool, error) {
	if !e.shouldTryBackupUnsafeReorg() {
		// Do not need to perform FCU.
		return false, nil
	}
	// Only try FCU once because execution engine may forgot backupUnsafeHead
	// or backupUnsafeHead is not part of the chain.
	// Exception: Retry when forkChoiceUpdate returns non-input error.
	e.needFCUCallForBackupUnsafeReorg = false
	// Reorg unsafe chain. Safe/Finalized chain will not be updated.
	e.log.Warn("trying to restore unsafe head", "backupUnsafe", e.backupUnsafeHead.ID(), "unsafe", e.unsafeHead.ID())
	fc := eth.ForkchoiceState{
		HeadBlockHash:      e.backupUnsafeHead.Hash,
		SafeBlockHash:      e.safeHead.Hash,
		FinalizedBlockHash: e.finalizedHead.Hash,
	}
	logFn := e.logSyncProgressMaybe()
	defer logFn()
	fcRes, err := e.engine.ForkchoiceUpdate(ctx, &fc, nil)
	if err != nil {
		var inputErr eth.InputError
		if errors.As(err, &inputErr) {
			e.SetBackupUnsafeL2Head(eth.L2BlockRef{}, false)
			switch inputErr.Code {
			case eth.InvalidForkchoiceState:
				return true, derive.NewResetError(fmt.Errorf("forkchoice update was inconsistent with engine, need reset to resolve: %w", inputErr.Unwrap()))
			default:
				return true, derive.NewTemporaryError(fmt.Errorf("unexpected error code in forkchoice-updated response: %w", err))
			}
		} else {
			// Retry when forkChoiceUpdate returns non-input error.
			// Do not reset backupUnsafeHead because it will be used again.
			e.needFCUCallForBackupUnsafeReorg = true
			return true, derive.NewTemporaryError(fmt.Errorf("failed to sync forkchoice with engine: %w", err))
		}
	}
	if fcRes.PayloadStatus.Status == eth.ExecutionValid {
		e.emitter.Emit(ForkchoiceUpdateEvent{
			UnsafeL2Head:    e.backupUnsafeHead,
			SafeL2Head:      e.safeHead,
			FinalizedL2Head: e.finalizedHead,
		})
		// Execution engine accepted the reorg.
		e.log.Info("successfully reorged unsafe head using backupUnsafe", "unsafe", e.backupUnsafeHead.ID())
		e.SetUnsafeHead(e.BackupUnsafeL2Head())
		e.SetBackupUnsafeL2Head(eth.L2BlockRef{}, false)
		return true, nil
	}
	e.SetBackupUnsafeL2Head(eth.L2BlockRef{}, false)
	// Execution engine could not reorg back to previous unsafe head.
	return true, derive.NewTemporaryError(fmt.Errorf("cannot restore unsafe chain using backupUnsafe: err: %w",
		eth.ForkchoiceUpdateErr(fcRes.PayloadStatus)))
}
