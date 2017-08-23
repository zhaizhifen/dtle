package models

import (
	gonats "github.com/nats-io/go-nats"
)

type TableStats struct {
	InsertCount int64
	UpdateCount int64
	DelCount    int64
}

type DelayCount struct {
	Num  uint64
	Time uint64
}

type ThroughputStat struct {
	Num  uint64
	Time uint64
}

type MsgStat struct {
	InMsgs   uint64
	OutMsgs  uint64
	InBytes  uint64
	OutBytes uint64
}

type BufferStat struct {
	ExtractorTxQueueSize    int
	ApplierTxQueueSize      int
	ApplierGroupTxQueueSize int
	SendByTimeout           int
	SendBySizeFull          int
}

type CurrentCoordinates struct {
	File     string
	Position int64
	GtidSet  string

	RelayMasterLogFile string
	ReadMasterLogPos   int64
	RetrievedGtidSet   string
	ExecutedGtidSet    string
}

type TaskStatistics struct {
	CurrentCoordinates *CurrentCoordinates
	TableStats         *TableStats
	DelayCount         *DelayCount
	RowsCount          int
	ThroughputStat     *ThroughputStat
	MsgStat            gonats.Statistics
	BufferStat         BufferStat
	Status             string
	Timestamp          int64
}

type AllocStatistics struct {
	Tasks map[string]*TaskStatistics
}
