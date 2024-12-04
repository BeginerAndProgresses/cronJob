//go:generate mockgen -destination=mocks/distributed.go -package=gocronmocks . Elector,Locker,Lock
package cron

import (
	"context"
)

// Elector 从请求成为领导者的实例中确定领导者。
// 只有领导者负责 Jobs。如果领导者倒下，将选出新的领导者。
type Elector interface {
	// IsLeader 如果作业应由实例调度，应返回 nil
	// 发出请求，如果不应调度作业，则返回错误
	IsLeader(context.Context) error
}

// Locker 表示在运行多个调度程序时锁定作业所需的接口。
// 该锁在作业运行期间保持，预计 locker 实现会处理 scheduler 之间的时间展开。
// 传递的锁定键是作业的名称 - 如果未设置，则默认为 go 函数的名称，
// 例如 pkg 中 func myJob（） {} 的 “pkg.myJob”
type Locker interface {
	// Lock 如果 Lock 返回错误，则不会调度作业。
	Lock(ctx context.Context, key string) (Lock, error)
}

// Lock 表示获取的锁。在计划程序执行作业后释放锁。
type Lock interface {
	Unlock(ctx context.Context) error
}
