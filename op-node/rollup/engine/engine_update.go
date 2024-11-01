package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum/go-ethereum/core/types"
)

// isDepositTx checks an opaqueTx to determine if it is a Deposit Transaction
// It has to return an error in the case the transaction is empty
func isDepositTx(opaqueTx eth.Data) (bool, error) {
	if len(opaqueTx) == 0 {
		return false, errors.New("empty transaction")
	}
	return opaqueTx[0] == types.DepositTxType, nil
}

// lastDeposit finds the index of last deposit at the start of the transactions.
// It walks the transactions from the start until it finds a non-deposit tx.
// An error is returned if any looked at transaction cannot be decoded
func lastDeposit(txns []eth.Data) (int, error) {
	var lastDeposit int
	for i, tx := range txns {
		deposit, err := isDepositTx(tx)
		if err != nil {
			return 0, fmt.Errorf("invalid transaction at idx %d", i)
		}
		if deposit {
			lastDeposit = i
		} else {
			break
		}
	}
	return lastDeposit, nil
}

func sanityCheckPayload(payload *eth.ExecutionPayload) error {
	// Sanity check payload before inserting it
	if len(payload.Transactions) == 0 {
		return errors.New("no transactions in returned payload")
	}
	if payload.Transactions[0][0] != types.DepositTxType {
		return fmt.Errorf("first transaction was not deposit tx. Got %v", payload.Transactions[0][0])
	}
	// Ensure that the deposits are first
	lastDeposit, err := lastDeposit(payload.Transactions)
	if err != nil {
		return fmt.Errorf("failed to find last deposit: %w", err)
	}
	// Ensure no deposits after last deposit
	for i := lastDeposit + 1; i < len(payload.Transactions); i++ {
		tx := payload.Transactions[i]
		deposit, err := isDepositTx(tx)
		if err != nil {
			return fmt.Errorf("failed to decode transaction idx %d: %w", i, err)
		}
		if deposit {
			return fmt.Errorf("deposit tx (%d) after other tx in l2 block with prev deposit at idx %d", i, lastDeposit)
		}
	}
	return nil
}

var ErrEngineSyncing = errors.New("engine is syncing")

type BlockInsertionErrType uint

const (
	// BlockInsertOK indicates that the payload was successfully executed and appended to the canonical chain.
	BlockInsertOK BlockInsertionErrType = iota
	// BlockInsertTemporaryErr indicates that the insertion failed but may succeed at a later time without changes to the payload.
	BlockInsertTemporaryErr
	// BlockInsertPrestateErr indicates that the pre-state to insert the payload could not be prepared, e.g. due to missing chain data.
	BlockInsertPrestateErr
	// BlockInsertPayloadErr indicates that the payload was invalid and cannot become canonical.
	BlockInsertPayloadErr
)

// startPayload starts an execution payload building process in the provided Engine, with the given attributes.
// The severity of the error is distinguished to determine whether the same payload attributes may be re-attempted later.
// startPayload 使用给定的属性在提供的引擎中启动执行有效负载构建过程。
// 区分错误的严重程度，以确定是否可以稍后重新尝试相同的有效负载属性。
// 在执行引擎中启动一个新的执行有效载荷（payload）构建过程。
// 使用给定的分叉选择状态（ForkchoiceState）和有效载荷属性（PayloadAttributes）来更新执行引擎的状态。
// 根据执行引擎的响应，判断有效载荷构建是否成功，并返回相应的结果或错误。
// 对不同类型的错误进行分类，以便调用者可以决定是否可以稍后重试相同的有效载荷属性。
func startPayload(ctx context.Context, eng ExecEngine, fc eth.ForkchoiceState, attrs *eth.PayloadAttributes) (id eth.PayloadID, errType BlockInsertionErrType, err error) {
	// 在 L2 系统的执行流程中扮演着关键角色。它主要用于更新执行引擎的分叉选择状态，并可能启动新区块的构建过程。
	// ForkchoiceUpdate 调用会将最新的分叉选择状态（包括当前的头部区块、安全区块和最终确认区块）发送给执行引擎。
	fcRes, err := eng.ForkchoiceUpdate(ctx, &fc, attrs)
	if err != nil {
		var inputErr eth.InputError
		if errors.As(err, &inputErr) {
			switch inputErr.Code {
			case eth.InvalidForkchoiceState:
				return eth.PayloadID{}, BlockInsertPrestateErr, fmt.Errorf("pre-block-creation forkchoice update was inconsistent with engine, need reset to resolve: %w", inputErr.Unwrap())
			case eth.InvalidPayloadAttributes:
				return eth.PayloadID{}, BlockInsertPayloadErr, fmt.Errorf("payload attributes are not valid, cannot build block: %w", inputErr.Unwrap())
			default:
				if inputErr.Code.IsEngineError() {
					return eth.PayloadID{}, BlockInsertPrestateErr, fmt.Errorf("unexpected engine error code in forkchoice-updated response: %w", err)
				} else {
					return eth.PayloadID{}, BlockInsertTemporaryErr, fmt.Errorf("unexpected generic error code in forkchoice-updated response: %w", err)
				}
			}
		} else {
			return eth.PayloadID{}, BlockInsertTemporaryErr, fmt.Errorf("failed to create new block via forkchoice: %w", err)
		}
	}

	switch fcRes.PayloadStatus.Status {
	// TODO: snap sync - specify explicit different error type if node is syncing
	case eth.ExecutionInvalid, eth.ExecutionInvalidBlockHash:
		return eth.PayloadID{}, BlockInsertPayloadErr, eth.ForkchoiceUpdateErr(fcRes.PayloadStatus)
	case eth.ExecutionValid:
		id := fcRes.PayloadID
		if id == nil {
			return eth.PayloadID{}, BlockInsertTemporaryErr, errors.New("nil id in forkchoice result when expecting a valid ID")
		}
		return *id, BlockInsertOK, nil
	case eth.ExecutionSyncing:
		return eth.PayloadID{}, BlockInsertTemporaryErr, ErrEngineSyncing
	default:
		return eth.PayloadID{}, BlockInsertTemporaryErr, eth.ForkchoiceUpdateErr(fcRes.PayloadStatus)
	}
}
