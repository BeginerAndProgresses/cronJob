package cron

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/google/uuid"
)

type executor struct {
	// ctx 用于关闭的上下文
	ctx context.Context
	// cancel 由 Executor 用于发出停止其函数的信号
	cancel context.CancelFunc
	// clock 用于常规时间或模拟时间
	clock clockwork.Clock
	// logger 记录日志
	logger Logger

	// 接收计划执行的作业
	jobsIn chan jobIn
	// 发送作业以进行重新调度
	jobsOutForRescheduling chan uuid.UUID
	// 完成后发送作业
	jobsOutCompleted chan uuid.UUID
	// 用于从调度程序请求作业
	jobOutRequest chan jobOutRequest

	// 由 Executor 用于从调度器接收 STOP 信号
	stopCh chan struct{}
	// 停止时的超时值
	stopTimeout time.Duration
	// 用于指示 Executor 已完成 shutdown
	done chan error

	// 任何单例类型作业的运行程序
	// map[uuid.UUID]singletonRunner
	singletonRunners *sync.Map
	// limit 模式的配置
	limitMode *limitModeConfig
	// 运行分布式实例时的 Elector
	elector Elector
	// 运行分布式实例时的储物柜
	locker Locker
	// 监控报告指标
	monitor Monitor
}

type jobIn struct {
	id            uuid.UUID
	shouldSendOut bool
}

type singletonRunner struct {
	in                chan jobIn
	rescheduleLimiter chan struct{}
}

type limitModeConfig struct {
	started           bool
	mode              LimitMode
	limit             uint
	rescheduleLimiter chan struct{}
	in                chan jobIn
	// singletonJobs 用于跟踪在限制模式运行程序中运行的单例作业。这用于防止在同时启用限制模式和单例模式时，同一作业在限制模式运行程序之间多次运行。
	singletonJobs   map[uuid.UUID]struct{}
	singletonJobsMu sync.Mutex
}

func (e *executor) start() {
	e.logger.Debug("gocron: executor started")

	// 在此处创建 executor 的 context 作为 executor 是唯一应该访问此上下文的 goroutine，
	// 在 executor 中的任何其他使用都应该使用 executor 上下文作为父级创建一个上下文。
	e.ctx, e.cancel = context.WithCancel(context.Background())

	standardJobsWg := &waitGroupWithMutex{}

	singletonJobsWg := &waitGroupWithMutex{}

	limitModeJobsWg := &waitGroupWithMutex{}

	// 创建一个新映射以跟踪单例运行程序
	e.singletonRunners = &sync.Map{}

	// 启动 for 循环，即执行程序
	// 选择 Work To Do 的频道
	for {
		select {
		// job的id从两个地方发出:
		// 1. 调度程序在 Job 立即运行。
		// 2. 发送自时间。作业调度的 AfterFuncs 由调度程序启动
		case jIn := <-e.jobsIn:
			select {
			case <-e.stopCh:
				e.stop(standardJobsWg, singletonJobsWg, limitModeJobsWg)
				return
			default:
			}
			// 此上下文用于处理 Executor 的取消
			// 通过 requestJobCtx 向调度程序请求作业
			ctx, cancel := context.WithCancel(e.ctx)

			if e.limitMode != nil && !e.limitMode.started {
				// 检查我们是否已经在运行 Limit Mode Runners
				// 如果没有，请增加所需的数量，即 Limit！
				e.limitMode.started = true
				for i := e.limitMode.limit; i > 0; i-- {
					limitModeJobsWg.Add(1)
					go e.limitModeRunner("limitMode-"+strconv.Itoa(int(i)), e.limitMode.in, limitModeJobsWg, e.limitMode.mode, e.limitMode.rescheduleLimiter)
				}
			}

			// 旋转到 goroutine 中以取消对 Executor 的阻塞，并且
			// 允许处理更多工作
			go func() {
				// 确保根据文档取消上述上下文
				// 取消此上下文会释放与其关联的资源，因此代码应
				// 在此 Context 中运行的操作完成后立即调用 cancel。
				defer cancel()

				// 检查限制模式 - 这将启动一个单独的运行程序，该运行程序处理
				// 限制并发运行的作业总数
				if e.limitMode != nil {
					if e.limitMode.mode == LimitModeReschedule {
						select {
						// rescheduleLimiter 是 limit 大小的通道
						// 这会阻止发布到频道，并将执行程序建立等待队列
						// 并强制重新安排
						case e.limitMode.rescheduleLimiter <- struct{}{}:
							e.limitMode.in <- jIn
						default:
							// all runners are busy, reschedule the work for later
							// which means we just skip it here and do nothing
							// TODO when metrics are added, this should increment a rescheduled metric
							e.sendOutForRescheduling(&jIn)
						}
					} else {
						// 因为我们没有使用 LimitModeReschedule，而是使用 LimitModeWait
						// 我们确实希望将工作排队到 Limit 模式运行程序并允许它们
						// 以处理通道积压工作。存在 1000 个硬性限制
						// 此时，此调用将阻止。
						// TODO 时，这应该会增加一个等待指标
						e.sendOutForRescheduling(&jIn)
						e.limitMode.in <- jIn
					}
				} else {
					// no limit 模式，因此我们要么运行常规作业，要么运行具有单例模式的作业
					//
					// 得到这份工作，这样我们就可以弄清楚它是什么类型以及如何执行它
					j := requestJobCtx(ctx, jIn.id, e.jobOutRequest)
					if j == nil {
						// safety check as it'd be strange bug if this occurred
						return
					}
					if j.singletonMode {
						// 对于 Singleton Mode （单例模式），获取作业的现有 Runner或启动一个新的
						runner := &singletonRunner{}
						runnerSrc, ok := e.singletonRunners.Load(jIn.id)
						if !ok {
							runner.in = make(chan jobIn, 1000)
							if j.singletonLimitMode == LimitModeReschedule {
								runner.rescheduleLimiter = make(chan struct{}, 1)
							}
							e.singletonRunners.Store(jIn.id, runner)
							singletonJobsWg.Add(1)
							go e.singletonModeRunner("singleton-"+jIn.id.String(), runner.in, singletonJobsWg, j.singletonLimitMode, runner.rescheduleLimiter)
						} else {
							runner = runnerSrc.(*singletonRunner)
						}

						if j.singletonLimitMode == LimitModeReschedule {
							// Reschedule 模式使用 Limiter 通道检查
							// 对于正在运行的作业，如果通道已满，则重新调度。
							select {
							case runner.rescheduleLimiter <- struct{}{}:
								runner.in <- jIn
								e.sendOutForRescheduling(&jIn)
							default:
								// Runner 很忙，请重新安排工作以备后用
								// 这意味着我们只是在这里跳过它，什么都不做
								e.incrementJobCounter(*j, SingletonRescheduled)
								e.sendOutForRescheduling(&jIn)
							}
						} else {
							// 等待模式，填满该队列 （buffered channel，所以没关系）
							runner.in <- jIn
							e.sendOutForRescheduling(&jIn)
						}
					} else {
						select {
						case <-e.stopCh:
							e.stop(standardJobsWg, singletonJobsWg, limitModeJobsWg)
							return
						default:
						}
						// 我们已经进入了 Basic / Standard Jobs ——
						// 那些没有什么特别的，只是想要的
						// 运行。添加到 WaitGroup，以便
						// stopping 或 Stopping down 可以等待作业完成。
						standardJobsWg.Add(1)
						go func(j internalJob) {
							e.runJob(j, jIn)
							standardJobsWg.Done()
						}(*j)
					}
				}
			}()
		case <-e.stopCh:
			e.stop(standardJobsWg, singletonJobsWg, limitModeJobsWg)
			return
		}
	}
}

func (e *executor) sendOutForRescheduling(jIn *jobIn) {
	if jIn.shouldSendOut {
		select {
		case e.jobsOutForRescheduling <- jIn.id:
		case <-e.ctx.Done():
			return
		}
	}
	// 我们现在需要将其设置为 false，因为要处理non-limit 作业，
	// 我们从 e.runJob 函数发送在这种情况下，我们不想发送两次。
	jIn.shouldSendOut = false
}

func (e *executor) limitModeRunner(name string, in chan jobIn, wg *waitGroupWithMutex, limitMode LimitMode, rescheduleLimiter chan struct{}) {
	e.logger.Debug("gocron: limitModeRunner starting", "name", name)
	for {
		select {
		case jIn := <-in:
			select {
			case <-e.ctx.Done():
				e.logger.Debug("gocron: limitModeRunner shutting down", "name", name)
				wg.Done()
				return
			default:
			}

			ctx, cancel := context.WithCancel(e.ctx)
			j := requestJobCtx(ctx, jIn.id, e.jobOutRequest)
			cancel()
			if j != nil {
				if j.singletonMode {
					e.limitMode.singletonJobsMu.Lock()
					_, ok := e.limitMode.singletonJobs[jIn.id]
					if ok {
						//此作业已在运行，因此不能重复运行它，但是能重新调度它
						e.limitMode.singletonJobsMu.Unlock()
						if jIn.shouldSendOut {
							select {
							case <-e.ctx.Done():
								return
							case <-j.ctx.Done():
								return
							case e.jobsOutForRescheduling <- j.id:
							}
						}
						// 删除 Limiter 块，因为这个特定的工作
						// 的单例已经在运行，我们希望
						// 允许安排其他作业
						if limitMode == LimitModeReschedule {
							<-rescheduleLimiter
						}
						continue
					}
					e.limitMode.singletonJobs[jIn.id] = struct{}{}
					e.limitMode.singletonJobsMu.Unlock()
				}
				e.runJob(*j, jIn)

				if j.singletonMode {
					e.limitMode.singletonJobsMu.Lock()
					delete(e.limitMode.singletonJobs, jIn.id)
					e.limitMode.singletonJobsMu.Unlock()
				}
			}

			// 删除 Limiter 块以允许调度另一个作业
			if limitMode == LimitModeReschedule {
				<-rescheduleLimiter
			}
		case <-e.ctx.Done():
			e.logger.Debug("limitModeRunner 关闭", "name", name)
			wg.Done()
			return
		}
	}
}

func (e *executor) singletonModeRunner(name string, in chan jobIn, wg *waitGroupWithMutex, limitMode LimitMode, rescheduleLimiter chan struct{}) {
	e.logger.Debug("gocron： singletonModeRunner 正在启动", "name", name)
	for {
		select {
		case jIn := <-in:
			select {
			case <-e.ctx.Done():
				e.logger.Debug("gocron：singletonModeRunner 正在关闭", "name", name)
				wg.Done()
				return
			default:
			}

			ctx, cancel := context.WithCancel(e.ctx)
			j := requestJobCtx(ctx, jIn.id, e.jobOutRequest)
			cancel()
			if j != nil {
				// 这里需要设置 shouldSendOut = false，因为在 runJob 函数内部有一个对 sendOutForRescheduling 的重复调用，需要跳过。
				// sendOutForRescheduling 之前被调用。
				// 当作业被发送到单例模式运行程序时，会调用 sendOutForRescheduling。
				jIn.shouldSendOut = false
				e.runJob(*j, jIn)
			}

			// 移除限制器块以允许调度另一个作业
			if limitMode == LimitModeReschedule {
				<-rescheduleLimiter
			}
		case <-e.ctx.Done():
			e.logger.Debug("singletonModeRunner关闭", "name", name)
			wg.Done()
			return
		}
	}
}

func (e *executor) runJob(j internalJob, jIn jobIn) {
	if j.ctx == nil {
		return
	}
	select {
	case <-e.ctx.Done():
		return
	case <-j.ctx.Done():
		return
	default:
	}

	if j.stopTimeReached(e.clock.Now()) {
		return
	}

	if e.elector != nil {
		if err := e.elector.IsLeader(j.ctx); err != nil {
			e.sendOutForRescheduling(&jIn)
			e.incrementJobCounter(j, Skip)
			return
		}
	} else if j.locker != nil {
		lock, err := j.locker.Lock(j.ctx, j.name)
		if err != nil {
			_ = callJobFuncWithParams(j.afterLockError, j.id, j.name, err)
			e.sendOutForRescheduling(&jIn)
			e.incrementJobCounter(j, Skip)
			return
		}
		defer func() { _ = lock.Unlock(j.ctx) }()
	} else if e.locker != nil {
		lock, err := e.locker.Lock(j.ctx, j.name)
		if err != nil {
			_ = callJobFuncWithParams(j.afterLockError, j.id, j.name, err)
			e.sendOutForRescheduling(&jIn)
			e.incrementJobCounter(j, Skip)
			return
		}
		defer func() { _ = lock.Unlock(j.ctx) }()
	}
	_ = callJobFuncWithParams(j.beforeJobRuns, j.id, j.name)

	e.sendOutForRescheduling(&jIn)
	select {
	case e.jobsOutCompleted <- j.id:
	case <-e.ctx.Done():
	}

	startTime := time.Now()
	err := e.callJobWithRecover(j)
	e.recordJobTiming(startTime, time.Now(), j)
	if err != nil {
		_ = callJobFuncWithParams(j.afterJobRunsWithError, j.id, j.name, err)
		e.incrementJobCounter(j, Fail)
	} else {
		_ = callJobFuncWithParams(j.afterJobRuns, j.id, j.name)
		e.incrementJobCounter(j, Success)
	}
}

func (e *executor) callJobWithRecover(j internalJob) (err error) {
	defer func() {
		if recoverData := recover(); recoverData != nil {
			_ = callJobFuncWithParams(j.afterJobRunsWithPanic, j.id, j.name, recoverData)

			// 如果发生panic，我们应该返回一个错误
			err = fmt.Errorf("%w from %v", ErrPanicRecovered, recoverData)
		}
	}()

	return callJobFuncWithParams(j.function, j.parameters...)
}

func (e *executor) recordJobTiming(start time.Time, end time.Time, j internalJob) {
	if e.monitor != nil {
		e.monitor.RecordJobTiming(start, end, j.id, j.name, j.tags)
	}
}

func (e *executor) incrementJobCounter(j internalJob, status JobStatus) {
	if e.monitor != nil {
		e.monitor.IncrementJob(j.id, j.name, j.tags, status)
	}
}

func (e *executor) stop(standardJobsWg, singletonJobsWg, limitModeJobsWg *waitGroupWithMutex) {
	e.logger.Debug("gocron: 停止执行器")
	// 我们被要求停止。这要么是因为调度器被告知
	// 停止所有作业，或者调度器被要求完全关闭。
	//
	// cancel告诉所有函数停止工作并发送一个done响应
	e.cancel()

	// waitForJobs 用于报告我们是否成功等待
	// 所有作业是否完成，或者是否达到了配置的超时时间。
	waitForJobs := make(chan struct{}, 1)
	waitForSingletons := make(chan struct{}, 1)
	waitForLimitMode := make(chan struct{}, 1)

	// waiter上下文用于取消等待job的函数。
	// 这样做是为了避免goroutine泄漏。
	waiterCtx, waiterCancel := context.WithCancel(context.Background())

	// 等待标准作业完成
	go func() {
		e.logger.Debug("gocron: 等待标准作业完成")
		go func() {
			// 这是在单独的goroutine中完成的，所以我们不是
			// 被事件中的WaitGroup的Wait调用阻塞
			// 提示waiter上下文被取消
			// 一些长时间运行的标准作业没有完成
			// 这个特定的goroutine可能会泄漏
			standardJobsWg.Wait()
			e.logger.Debug("gocron: 已完成的标准作业")
			waitForJobs <- struct{}{}
		}()
		<-waiterCtx.Done()
	}()

	// 等待每个作业单例限制模式运行程序作业完成
	go func() {
		e.logger.Debug("gocron: waiting for singleton jobs to complete")
		go func() {
			singletonJobsWg.Wait()
			e.logger.Debug("gocron: singleton jobs completed")
			waitForSingletons <- struct{}{}
		}()
		<-waiterCtx.Done()
	}()

	// 等待极限模式运行完成
	go func() {
		e.logger.Debug("gocron: 等待极限模式运行完成")
		go func() {
			limitModeJobsWg.Wait()
			e.logger.Debug("gocron: 任务完成")
			waitForLimitMode <- struct{}{}
		}()
		<-waiterCtx.Done()
	}()

	// 现在要么等待所有作业完成，
	// 要么触发timeout
	var count int
	timeout := time.Now().Add(e.stopTimeout)
	for time.Now().Before(timeout) && count < 3 {
		select {
		case <-waitForJobs:
			count++
		case <-waitForSingletons:
			count++
		case <-waitForLimitMode:
			count++
		default:
		}
	}
	if count < 3 {
		e.done <- ErrStopJobsTimedOut
		e.logger.Debug("gocron: 执行器停止-超时")
	} else {
		e.done <- nil
		e.logger.Debug("gocron: 执行器停止了")
	}
	waiterCancel()

	if e.limitMode != nil {
		e.limitMode.started = false
	}
}
