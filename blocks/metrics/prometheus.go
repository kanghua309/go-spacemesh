package metrics

import (
	"github.com/spacemeshos/go-spacemesh/metrics"
)

const (
	subsystem     = "blocks"
	diffTypeLabel = "diff_type"
)

// AvgLayerBlockSize checks average block size.
var AvgLayerBlockSize = metrics.NewHistogram(
	"avg_layer_block_size",
	subsystem,
	"Average block size",
	[]string{
		"layer_id",
		"block_id",
	},
)

// AvgNumTxsInBlock checks average transaction count in block.
var AvgNumTxsInBlock = metrics.NewHistogram(
	"avg_num_txs_in_block",
	subsystem,
	"Average number of transactions in block",
	[]string{
		"layer_id",
		"block_id",
	},
)

var avgBaseBlockExceptionLength = metrics.NewHistogram(
	"avg_base_block_exception_length",
	subsystem,
	"Average base block exception length",
	[]string{
		"layer_id",
		"block_id",
		diffTypeLabel,
	},
)

// Average blocks diff lengths.
var (
	AvgForDiffLength     = avgBaseBlockExceptionLength.With(diffTypeLabel, "for_diff")
	AvgAgainstDiffLength = avgBaseBlockExceptionLength.With(diffTypeLabel, "against_diff")
	AvgNeutralDiffLength = avgBaseBlockExceptionLength.With(diffTypeLabel, "neutral_diff")
)