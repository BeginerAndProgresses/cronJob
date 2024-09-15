package cron

import (
	"time"

	"github.com/google/uuid"
)

// JobStatus 是应与指标一起收集的任务运行的状态。
type JobStatus string

// 可以使用的Job的不同状态。
const (
	Fail                 JobStatus = "fail"
	Success              JobStatus = "success"
	Skip                 JobStatus = "skip"
	SingletonRescheduled JobStatus = "singleton_rescheduled"
)

// Monitor 表示用于收集 jobs 指标的接口。
type Monitor interface {
	// IncrementJob 将提供有关作业的详细信息，并期望底层实现
	// 处理实例化和递增值
	IncrementJob(id uuid.UUID, name string, tags []string, status JobStatus)
	// RecordJobTiming 将提供有关作业和时间的详细信息，并期望底层实现
	// 处理实例化和记录值
	RecordJobTiming(startTime, endTime time.Time, id uuid.UUID, name string, tags []string)
}
