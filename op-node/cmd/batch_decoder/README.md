# Batch Decoding Tool

The batch decoding tool is a utility to aid in debugging the batch submitter & the op-node
by looking at what batches were submitted on L1.
批量解码工具是一种实用程序，通过查看在 L1 上提交了哪些批次来帮助调试批量提交者和操作节点。

## Design Philosophy

The `batch_decoder` tool is designed to be simple & flexible. It offloads as much data analysis
as possible to other tools. It is built around manipulating JSON on disk. The first stage is to
fetch all transactions which are sent to a batch inbox address. Those transactions are decoded into
frames in that step & information about them is recorded. After transactions are fetched the frames
are re-assembled into channels in a second step that does not touch the network.

`batch_decoder` 工具设计简单灵活。它将尽可能多的数据分析卸载到其他工具。它围绕在磁盘上操作 JSON 构建。
第一阶段是获取发送到批量收件箱地址的所有交易。这些交易在该步骤中被解码为帧，并记录有关它们的信息。获取交易后，帧在不接触网络的第二步中重新组装到通道中。

## Commands

### Fetch

`batch_decoder fetch` pulls all L1 transactions sent to the batch inbox address in a given L1 block
range and then stores them on disk to a specified path as JSON files where the name of the file is
the transaction hash.
`batch_decoder fetch` 提取发送到给定 L1 块范围内的批处理收件箱地址的所有 L1 交易，然后将它们作为 JSON 文件存储在磁盘上的指定路径中，其中文件的名称是交易哈希。


### Reassemble

`batch_decoder reassemble` goes through all of the found frames in the cache & then turns them
into channels. It then stores the channels with metadata on disk where the file name is the Channel ID.
Each channel can contain multiple batches.

If the batch is span batch, `batch_decoder` derives span batch using `L2BlockTime`, `L2GenesisTime`, and `L2ChainID`.
These arguments can be provided to the binary using flags.

If the batch is a singular batch, `batch_decoder` does not derive and stores the batch as is.

`batch_decoder reassemble` 遍历缓存中找到的所有帧，然后将它们转换为通道。然后，它将通道与元数据一起存储在磁盘上，其中文件名是通道 ID。
每个通道可以包含多个批次。

如果批次是跨批次，则 `batch_decoder` 使用 `L2BlockTime`、`L2GenesisTime` 和 `L2ChainID` 派生跨批次。
可以使用标志将这些参数提供给二进制文件。

如果批次是单个批次，则 `batch_decoder` 不会派生并按原样存储批次。

### Force Close

`batch_decoder force-close` will create a transaction data that can be sent from the batcher address to
the batch inbox address which will force close the given channels. This will allow future channels to
be read without waiting for the channel timeout. It uses the results from `batch_decoder fetch` to
create the close transaction because the transaction it creates for a specific channel requires information
about if the channel has been closed or not. If it has been closed already but is missing specific frames
those frames need to be generated differently than simply closing the channel.

`batch_decoder force-close` 将创建一个交易数据，该数据可以从批处理程序地址发送到批处理收件箱地址，从而强制关闭给定的通道。
这将允许读取未来的通道而无需等待通道超时。它使用来自 `batch_decoder fetch` 的结果来创建关闭交易，因为它为特定通道创建的交易需要有关通道是否已关闭的信息。
如果它已经关闭但缺少特定帧，则需要以不同于简单地关闭通道的方式生成这些帧。

## JQ Cheat Sheet

`jq` is a really useful utility for manipulating JSON files.

```
# Pretty print a JSON file
jq . $JSON_FILE

# Print the number of valid & invalid transactions
jq .valid_data $TX_DIR/* | sort | uniq -c

# Select all transactions that have invalid data & then print the transaction hash
jq "select(.valid_data == false)|.tx.hash" $TX_DIR

# Select all channels that are not ready and then get the id and inclusion block & tx hash of the first frame.
jq "select(.is_ready == false)|[.id, .frames[0].inclusion_block, .frames[0].transaction_hash]"  $CHANNEL_DIR

# Show all of the frames in a channel without seeing the batches or frame data
jq 'del(.batches)|del(.frames[]|.frame.data)' $CHANNEL_FILE

# Show all batches (without timestamps) in a channel
jq '.batches|del(.[]|.Transactions)' $CHANNEL_FILE
```


## Roadmap

- Pull the batches out of channels & store that information inside the ChannelWithMetadata (CLI-3565)
	- Transaction Bytes used
	- Total uncompressed (different from tx bytes) + compressed bytes
- Invert ChannelWithMetadata so block numbers/hashes are mapped to channels they are submitted in (CLI-3560)

## 路线图

- 将批次从通道中拉出并将该信息存储在 ChannelWithMetadata 中 (CLI-3565)
- 使用的交易字节数
- 总未压缩字节数（不同于 tx 字节数）+ 压缩字节数
- 反转 ChannelWithMetadata，以便将区块编号/哈希映射到它们提交的通道 (CLI-3560)


