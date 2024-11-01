package derive

import (
	"fmt"

	"github.com/hashicorp/go-multierror"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

// UserDeposits transforms the L2 block-height and L1 receipts into the transaction inputs for a full L2 block
func UserDeposits(receipts []*types.Receipt, depositContractAddr common.Address) ([]*types.DepositTx, error) {
	var out []*types.DepositTx
	var result error
	for i, rec := range receipts {
		if rec.Status != types.ReceiptStatusSuccessful {
			continue
		}
		for j, log := range rec.Logs {
			if log.Address == depositContractAddr && len(log.Topics) > 0 && log.Topics[0] == DepositEventABIHash {
				dep, err := UnmarshalDepositLogEvent(log)
				if err != nil {
					result = multierror.Append(result, fmt.Errorf("malformatted L1 deposit log in receipt %d, log %d: %w", i, j, err))
				} else {
					out = append(out, dep)
				}
			}
		}
	}
	return out, result
}

// DeriveDeposits 能是从 L1 的收据中提取用户存款交易，然后将这些交易编码为二进制格式，以便后续在 L2 中处理。这是 L1 到 L2 存款过程中的一个关键步骤，确保了用户在 L1 上发起的存款能够正确地在 L2 上得到处理。
func DeriveDeposits(receipts []*types.Receipt, depositContractAddr common.Address) ([]hexutil.Bytes, error) {
	var result error
	// 调用 UserDeposits 函数，从给定的收据中提取用户存款交易：
	userDeposits, err := UserDeposits(receipts, depositContractAddr)
	if err != nil {
		result = multierror.Append(result, err)
	}
	// 创建一个切片来存储编码后的交易：
	encodedTxs := make([]hexutil.Bytes, 0, len(userDeposits))
	// 遍历所有的用户存款交易，将每个交易编码为二进制格式：
	for i, tx := range userDeposits {
		opaqueTx, err := types.NewTx(tx).MarshalBinary()
		if err != nil {
			result = multierror.Append(result, fmt.Errorf("failed to encode user tx %d", i))
		} else {
			encodedTxs = append(encodedTxs, opaqueTx)
		}
	}
	// 返回编码后的交易列表和可能的错误：
	return encodedTxs, result
}
