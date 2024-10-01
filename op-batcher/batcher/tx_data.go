package batcher

import (
	"fmt"
	"strings"

	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

// txData represents the data for a single transaction.
// 用于表示和处理单个交易的数据
//
// Note: The batcher currently sends exactly one frame per transaction. This
// might change in the future to allow for multiple frames from possibly
// different channels.
// 注意：批处理程序当前每笔交易只发送一帧。这
// 将来可能会发生变化，以允许来自可能不同渠道的多个帧。
type txData struct {
	frames []frameData
	asBlob bool // indicates whether this should be sent as blob
}

// 创建一个包含单个帧的 txData 实例。
func singleFrameTxData(frame frameData) txData {
	return txData{frames: []frameData{frame}}
}

// ID returns the id for this transaction data. Its String() can be used as a map key.
// 返回该交易数据的 ID，作为 txID 类型。可以用其 String() 方法作为映射的键。
func (td *txData) ID() txID {
	id := make(txID, 0, len(td.frames))
	for _, f := range td.frames {
		id = append(id, f.id)
	}
	return id
}

// CallData returns the transaction data as calldata.
// It's a version byte (0) followed by the concatenated frames for this transaction.
// 返回交易数据作为 calldata。它是一个版本字节（0）后跟该交易的所有帧的连接。
func (td *txData) CallData() []byte {
	data := make([]byte, 1, 1+td.Len())
	data[0] = derive.DerivationVersion0
	for _, f := range td.frames {
		data = append(data, f.data...)
	}
	return data
}

// Blobs 返回交易数据的 Blob 列表。每个 Blob 包含一个版本字节（0）和帧数据。
func (td *txData) Blobs() ([]*eth.Blob, error) {
	blobs := make([]*eth.Blob, 0, len(td.frames))
	for _, f := range td.frames {
		var blob eth.Blob
		if err := blob.FromData(append([]byte{derive.DerivationVersion0}, f.data...)); err != nil {
			return nil, err
		}
		blobs = append(blobs, &blob)
	}
	return blobs, nil
}

// Len returns the sum of all the sizes of data in all frames.
// 返回所有帧中所有数据大小的总和。
// Len only counts the data itself and doesn't account for the version byte(s).
// 仅计算数据本身，不考虑版本字节。
func (td *txData) Len() (l int) {
	for _, f := range td.frames {
		l += len(f.data)
	}
	return l
}

// Frames returns the single frame of this tx data.
// 返回该交易数据的所有帧。
func (td *txData) Frames() []frameData {
	return td.frames
}

// txID is an opaque identifier for a transaction.
// txID 是交易的不透明标识符。
// Its internal fields should not be inspected after creation & are subject to change.
// 其内部字段在创建后不应被检查且可能会发生变化。
// Its String() can be used for comparisons and works as a map key.
// 其 String() 可用于比较并可用作映射键。
type txID []frameID

// 返回交易 ID 的字符串表示形式。
func (id txID) String() string {
	return id.string(func(id derive.ChannelID) string { return id.String() })
}

// TerminalString implements log.TerminalStringer, formatting a string for console
// output during logging.
// 实现 log.TerminalStringer 接口，用于在日志记录期间格式化字符串以供控制台输出。
func (id txID) TerminalString() string {
	return id.string(func(id derive.ChannelID) string { return id.TerminalString() })
}

// 辅助方法，用于生成交易 ID 的字符串表示形式。
func (id txID) string(chIDStringer func(id derive.ChannelID) string) string {
	var (
		sb      strings.Builder
		curChID derive.ChannelID
	)
	for _, f := range id {
		if f.chID == curChID {
			sb.WriteString(fmt.Sprintf("+%d", f.frameNumber))
		} else {
			if curChID != (derive.ChannelID{}) {
				sb.WriteString("|")
			}
			curChID = f.chID
			sb.WriteString(fmt.Sprintf("%s:%d", chIDStringer(f.chID), f.frameNumber))
		}
	}
	return sb.String()
}
