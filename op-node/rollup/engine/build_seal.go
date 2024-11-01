package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

// PayloadSealInvalidEvent identifies a permanent in-consensus problem with the payload sealing.
type PayloadSealInvalidEvent struct {
	Info eth.PayloadInfo
	Err  error

	IsLastInSpan bool
	DerivedFrom  eth.L1BlockRef
}

func (ev PayloadSealInvalidEvent) String() string {
	return "payload-seal-invalid"
}

// PayloadSealExpiredErrorEvent identifies a form of failed payload-sealing that is not coupled
// to the attributes themselves, but rather the build-job process.
// The user should re-attempt by starting a new build process. The payload-sealing job should not be re-attempted,
// as it most likely expired, timed out, or referenced an otherwise invalidated block-building job identifier.
type PayloadSealExpiredErrorEvent struct {
	Info eth.PayloadInfo
	Err  error

	IsLastInSpan bool
	DerivedFrom  eth.L1BlockRef
}

func (ev PayloadSealExpiredErrorEvent) String() string {
	return "payload-seal-expired-error"
}

type BuildSealEvent struct {
	Info         eth.PayloadInfo
	BuildStarted time.Time
	// if payload should be promoted to safe (must also be pending safe, see DerivedFrom)
	IsLastInSpan bool
	// payload is promoted to pending-safe if non-zero
	DerivedFrom eth.L1BlockRef
}

func (ev BuildSealEvent) String() string {
	return "build-seal"
}

// 处理区块封装事件
func (eq *EngDeriver) onBuildSeal(ev BuildSealEvent) {
	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(eq.ctx, buildSealTimeout)
	defer cancel()
	// 记录封装开始时间
	sealingStart := time.Now()
	// 从执行引擎获取 payload
	envelope, err := eq.ec.engine.GetPayload(ctx, ev.Info)
	if err != nil {
		// 处理 payload ID 未知的情况
		if x, ok := err.(eth.InputError); ok && x.Code == eth.UnknownPayload { //nolint:all
			eq.log.Warn("Cannot seal block, payload ID is unknown",
				"payloadID", ev.Info.ID, "payload_time", ev.Info.Timestamp,
				"started_time", ev.BuildStarted)
		}
		// 发出封装过期错误事件
		// Although the engine will very likely not be able to continue from here with the same building job,
		// we still call it "temporary", since the exact same payload-attributes have not been invalidated in-consensus.
		// So the user (attributes-handler or sequencer) should be able to re-attempt the exact
		// same attributes with a new block-building job from here to recover from this error.
		// We name it "expired", as this generally identifies a timeout, unknown job, or otherwise invalidated work.
		// 尽管引擎很可能无法从这里继续执行相同的构建作业，
		// 我们仍将其称为“临时”，因为完全相同的有效载荷属性尚未在共识中失效。
		// 因此，用户（属性处理程序或序列器）应该能够从这里重新尝试使用新的块构建作业执行完全相同的属性，以从此错误中恢复。
		// 我们将其命名为“已过期”，因为这通常表示超时、未知作业或其他无效工作。
		eq.emitter.Emit(PayloadSealExpiredErrorEvent{
			Info:         ev.Info,
			Err:          fmt.Errorf("failed to seal execution payload (ID: %s): %w", ev.Info.ID, err),
			IsLastInSpan: ev.IsLastInSpan,
			DerivedFrom:  ev.DerivedFrom,
		})
		return
	}
	// 执行 payload 完整性检查
	if err := sanityCheckPayload(envelope.ExecutionPayload); err != nil {
		eq.emitter.Emit(PayloadSealInvalidEvent{
			Info: ev.Info,
			Err: fmt.Errorf("failed sanity-check of execution payload contents (ID: %s, blockhash: %s): %w",
				ev.Info.ID, envelope.ExecutionPayload.BlockHash, err),
			IsLastInSpan: ev.IsLastInSpan,
			DerivedFrom:  ev.DerivedFrom,
		})
		return
	}
	// 将 payload 转换为区块引用
	ref, err := derive.PayloadToBlockRef(eq.cfg, envelope.ExecutionPayload)
	if err != nil {
		eq.emitter.Emit(PayloadSealInvalidEvent{
			Info:         ev.Info,
			Err:          fmt.Errorf("failed to decode L2 block ref from payload: %w", err),
			IsLastInSpan: ev.IsLastInSpan,
			DerivedFrom:  ev.DerivedFrom,
		})
		return
	}
	// 计算并记录性能指标
	now := time.Now()
	sealTime := now.Sub(sealingStart)
	buildTime := now.Sub(ev.BuildStarted)
	eq.metrics.RecordSequencerSealingTime(sealTime)
	eq.metrics.RecordSequencerBuildingDiffTime(buildTime - time.Duration(eq.cfg.BlockTime)*time.Second)
	// 记录交易数量
	txnCount := len(envelope.ExecutionPayload.Transactions)
	eq.metrics.CountSequencedTxs(txnCount)
	// 记录日志
	eq.log.Debug("Processed new L2 block", "l2_unsafe", ref, "l1_origin", ref.L1Origin,
		"txs", txnCount, "time", ref.Time, "seal_time", sealTime, "build_time", buildTime)
	// 发出区块封装完成事件
	eq.emitter.Emit(BuildSealedEvent{
		IsLastInSpan: ev.IsLastInSpan,
		DerivedFrom:  ev.DerivedFrom,
		Info:         ev.Info,
		Envelope:     envelope,
		Ref:          ref,
	})
}
