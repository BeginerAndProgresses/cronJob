//go:generate mockgen -destination=mocks/scheduler.go -package=gocronmocks . Scheduler
package cron

import (
	"context"
	"reflect"
	"runtime"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"golang.org/x/exp/slices"
)

var _ Scheduler = (*scheduler)(nil)

// Scheduler 定义 Scheduler 的接口。
type Scheduler interface {
	// Jobs 返回当前在调度程序中的所有作业。
	Jobs() []Job
	// NewJob 在调度器中创建一个新作业。
	// 在启动调度器时，根据提供的定义调度作业。
	// 如果调度器已经在运行，作业将在调度器启动时被调度。
	NewJob(JobDefinition, Task, ...JobOption) (Job, error)
	// RemoveByTags 删除所有具有提供的标签的作业。
	RemoveByTags(...string)
	// RemoveJob 删除指定 ID 的作业。
	RemoveJob(uuid.UUID) error
	// Shutdown 应该在您不再需要调度器或作业时调用，
	// 因为在调用Shutdown后无法重新启动调度器。
	// 这类似于关闭或清理方法，通常在启动调度器后延迟执行。
	Shutdown() error
	// Start 开始根据每个作业的定义来调度作业执行。
	// 作业添加到已经运行的调度器将立即根据定义进行调度。
	// 开始是非阻塞的。
	Start()
	// StopJobs 停止调度器中所有作业的执行。
	// 在需要全局暂停作业，然后用Start()重新启动作业的情况下，将会很有用。
	StopJobs() error
	// Update 根据一个作业的唯一标识符，将 JobDefinition 更新，并更新Task，
	// 原本task不会保留，更新后工作的唯一标识符不变。
	Update(uuid.UUID, JobDefinition, Task, ...JobOption) (Job, error)
	// JobsWaitingInQueue 队列中等待的作业数，在LimitModeWait情况下，
	// 在 limitmoderes schedule 情况下，或者没有限制时，它总是0
	JobsWaitingInQueue() int
}

// -----------------------------------------------
// -----------------------------------------------
// ------------------- 调度器 ---------------------
// -----------------------------------------------
// -----------------------------------------------

type scheduler struct {
	// 用于关闭的上下文
	shutdownCtx context.Context
	// cancel 用于向 Scheduler 发出 SWITCH DOWN 信号
	shutdownCancel context.CancelFunc
	// 执行器，实际运行通过调度器发送给它的作业
	exec executor
	// 在调度器中注册的作业的映射
	jobs map[uuid.UUID]internalJob
	// location 被用于与时间相关的所有调度操作的调度中
	location *time.Location
	// 调度器是否被启动
	started bool
	// 在添加到调度程序的所有作业上全局应用JobOption设置
	// 注意:单独设置的JobOption优先。
	globalJobOptions []JobOption
	// logger
	logger Logger

	// 作为一个信号，告诉调度器开始
	startCh chan struct{}
	// 用于报告调度程序已启动
	startedCh chan struct{}
	// 作为一个信号，告诉调度器停止
	stopCh chan struct{}
	// 用于报告调度程序停止
	stopErrCh chan error
	// 用于在客户端发出请求时发送所有作业
	allJobsOutRequest chan allJobsOutRequest
	// 用于在客户端发出请求时发送作业
	jobOutRequestCh chan jobOutRequest
	// 用于在客户端请求时按需运行作业
	runJobRequestCh chan runJobRequest
	// 在这里接收新工作
	newJobCh chan newJobIn
	// 在此处接收来自客户端的按 ID 删除作业的请求
	removeJobCh chan uuid.UUID
	// 此处收到客户通过标签删除作业的请求
	removeJobsByTagsCh chan []string
}

type newJobIn struct {
	ctx    context.Context
	cancel context.CancelFunc
	job    internalJob
}

type jobOutRequest struct {
	id      uuid.UUID
	outChan chan internalJob
}

type runJobRequest struct {
	id      uuid.UUID
	outChan chan error
}

type allJobsOutRequest struct {
	outChan chan []Job
}

// NewScheduler 创建一个新的调度器实例.
// 这个调度器是未启动状态，直到 Start() 被调用
//
// NewJob 会将作业添加到 Scheduler 中，但它们不会被调度
// 直到到调用 Start（） 为止。
func NewScheduler(options ...SchedulerOption) (Scheduler, error) {
	schCtx, cancel := context.WithCancel(context.Background())

	exec := executor{
		stopCh:           make(chan struct{}),
		stopTimeout:      time.Second * 10,
		singletonRunners: nil,
		logger:           &noOpLogger{},
		clock:            clockwork.NewRealClock(),

		jobsIn:                 make(chan jobIn),
		jobsOutForRescheduling: make(chan uuid.UUID),
		jobsOutCompleted:       make(chan uuid.UUID),
		jobOutRequest:          make(chan jobOutRequest, 1000),
		done:                   make(chan error),
	}

	s := &scheduler{
		shutdownCtx:    schCtx,
		shutdownCancel: cancel,
		exec:           exec,
		jobs:           make(map[uuid.UUID]internalJob),
		location:       time.Local,
		logger:         &noOpLogger{},

		newJobCh:           make(chan newJobIn),
		removeJobCh:        make(chan uuid.UUID),
		removeJobsByTagsCh: make(chan []string),
		startCh:            make(chan struct{}),
		startedCh:          make(chan struct{}),
		stopCh:             make(chan struct{}),
		stopErrCh:          make(chan error, 1),
		jobOutRequestCh:    make(chan jobOutRequest),
		runJobRequestCh:    make(chan runJobRequest),
		allJobsOutRequest:  make(chan allJobsOutRequest),
	}

	for _, option := range options {
		err := option(s)
		if err != nil {
			return nil, err
		}
	}

	go func() {
		s.logger.Info("gocron: 新的调度器已经创建")
		for {
			select {
			case id := <-s.exec.jobsOutForRescheduling:
				s.selectExecJobsOutForRescheduling(id)

			case id := <-s.exec.jobsOutCompleted:
				s.selectExecJobsOutCompleted(id)

			case in := <-s.newJobCh:
				s.selectNewJob(in)

			case id := <-s.removeJobCh:
				s.selectRemoveJob(id)

			case tags := <-s.removeJobsByTagsCh:
				s.selectRemoveJobsByTags(tags)

			case out := <-s.exec.jobOutRequest:
				s.selectJobOutRequest(out)

			case out := <-s.jobOutRequestCh:
				s.selectJobOutRequest(out)

			case out := <-s.allJobsOutRequest:
				s.selectAllJobsOutRequest(out)

			case run := <-s.runJobRequestCh:
				s.selectRunJobRequest(run)

			case <-s.startCh:
				s.selectStart()

			case <-s.stopCh:
				s.stopScheduler()

			case <-s.shutdownCtx.Done():
				s.stopScheduler()
				return
			}
		}
	}()

	return s, nil
}

// -----------------------------------------------
// -----------------------------------------------
// ------------ Scheduler 管道方法 ----------------
// -----------------------------------------------
// -----------------------------------------------

// 调度器的channel函数在这里被分解，允许在select块内进行优先级排序。
// 我们的想法是确保调度任务不会被调用者请求有关作业的信息所阻塞。

func (s *scheduler) stopScheduler() {
	s.logger.Debug("gocron: stopping scheduler")
	if s.started {
		s.exec.stopCh <- struct{}{}
	}

	for _, j := range s.jobs {
		j.stop()
	}
	for id, j := range s.jobs {
		<-j.ctx.Done()

		j.ctx, j.cancel = context.WithCancel(s.shutdownCtx)
		s.jobs[id] = j
	}
	var err error
	if s.started {
		select {
		case err = <-s.exec.done:
		case <-time.After(s.exec.stopTimeout + 1*time.Second):
			err = ErrStopExecutorTimedOut
		}
	}
	s.stopErrCh <- err
	s.started = false
	s.logger.Debug("gocron: scheduler stopped")
}

func (s *scheduler) selectAllJobsOutRequest(out allJobsOutRequest) {
	outJobs := make([]Job, len(s.jobs))
	var counter int
	for _, j := range s.jobs {
		outJobs[counter] = s.jobFromInternalJob(j)
		counter++
	}
	slices.SortFunc(outJobs, func(a, b Job) int {
		aID, bID := a.ID().String(), b.ID().String()
		switch {
		case aID < bID:
			return -1
		case aID > bID:
			return 1
		default:
			return 0
		}
	})
	select {
	case <-s.shutdownCtx.Done():
	case out.outChan <- outJobs:
	}
}

func (s *scheduler) selectRunJobRequest(run runJobRequest) {
	j, ok := s.jobs[run.id]
	if !ok {
		select {
		case run.outChan <- ErrJobNotFound:
		default:
		}
	}
	select {
	case <-s.shutdownCtx.Done():
		select {
		case run.outChan <- ErrJobRunNowFailed:
		default:
		}
	case s.exec.jobsIn <- jobIn{
		id:            j.id,
		shouldSendOut: false,
	}:
		select {
		case run.outChan <- nil:
		default:
		}
	}
}

func (s *scheduler) selectRemoveJob(id uuid.UUID) {
	j, ok := s.jobs[id]
	if !ok {
		return
	}
	j.stop()
	delete(s.jobs, id)
}

// 从执行器返回到调度程序的作业需要评估重新安排。
func (s *scheduler) selectExecJobsOutForRescheduling(id uuid.UUID) {
	select {
	case <-s.shutdownCtx.Done():
		return
	default:
	}
	j, ok := s.jobs[id]
	if !ok {
		// the job was removed while it was running, and
		// so we don't need to reschedule it.
		return
	}

	if j.stopTimeReached(s.now()) {
		return
	}

	scheduleFrom := j.lastRun
	if len(j.nextScheduled) > 0 {
		// 总是获取切片中的最后一个元素，因为它是未来最远的，以及我们希望计算后续下一次运行时的时间。
		slices.SortStableFunc(j.nextScheduled, ascendingTime)
		scheduleFrom = j.nextScheduled[len(j.nextScheduled)-1]
	}

	if scheduleFrom.IsZero() {
		scheduleFrom = j.startTime
	}

	next := j.next(scheduleFrom)
	if next.IsZero() {
		// the job's next function will return zero for OneTime jobs.
		// since they are one time only, they do not need rescheduling.
		return
	}

	if next.Before(s.now()) {
		// 在某些情况下，下一次运行时间可能是过去的时间，
		// 例如:机器上的时间不正确，并已与NTP同步—机器进入睡眠，并在一段时间后醒来
		// 在这种情况下，我们希望增加到未来的下一次运行，并为该时间安排任务。
		for next.Before(s.now()) {
			next = j.next(next)
		}
	}
	j.nextScheduled = append(j.nextScheduled, next)
	j.timer = s.exec.clock.AfterFunc(next.Sub(s.now()), func() {
		// 在这里设置作业的实际定时器，监听shutdown事件，这样当调度器关闭时，作业就不会尝试运行。
		select {
		case <-s.shutdownCtx.Done():
			return
		case s.exec.jobsIn <- jobIn{
			id:            j.id,
			shouldSendOut: true,
		}:
		}
	})
	// 更新作业下一次运行时间和最后一次运行时间
	s.jobs[id] = j
}

func (s *scheduler) selectExecJobsOutCompleted(id uuid.UUID) {
	j, ok := s.jobs[id]
	if !ok {
		return
	}

	// 如果作业的下一个计划时间是在过去，我们需要删除所有在过去的时间。
	var newNextScheduled []time.Time
	for _, t := range j.nextScheduled {
		if t.Before(s.now()) {
			continue
		}
		newNextScheduled = append(newNextScheduled, t)
	}
	j.nextScheduled = newNextScheduled

	// 如果作业设置了有限的运行次数，
	// 我们需要检查已经运行了多少次，并在达到限制时停止运行该作业。
	if j.limitRunsTo != nil {
		j.limitRunsTo.runCount = j.limitRunsTo.runCount + 1
		if j.limitRunsTo.runCount == j.limitRunsTo.limit {
			go func() {
				select {
				case <-s.shutdownCtx.Done():
					return
				case s.removeJobCh <- id:
				}
			}()
			return
		}
	}

	j.lastRun = s.now()
	s.jobs[id] = j
}

func (s *scheduler) selectJobOutRequest(out jobOutRequest) {
	if j, ok := s.jobs[out.id]; ok {
		select {
		case out.outChan <- j:
		case <-s.shutdownCtx.Done():
		}
	}
	close(out.outChan)
}

func (s *scheduler) selectNewJob(in newJobIn) {
	j := in.job
	if s.started {
		next := j.startTime
		if j.startImmediately {
			next = s.now()
			select {
			case <-s.shutdownCtx.Done():
			case s.exec.jobsIn <- jobIn{
				id:            j.id,
				shouldSendOut: true,
			}:
			}
		} else {
			if next.IsZero() {
				next = j.next(s.now())
			}

			id := j.id
			j.timer = s.exec.clock.AfterFunc(next.Sub(s.now()), func() {
				select {
				case <-s.shutdownCtx.Done():
				case s.exec.jobsIn <- jobIn{
					id:            id,
					shouldSendOut: true,
				}:
				}
			})
		}
		j.startTime = next
		j.nextScheduled = append(j.nextScheduled, next)
	}

	s.jobs[j.id] = j
	in.cancel()
}

func (s *scheduler) selectRemoveJobsByTags(tags []string) {
	for _, j := range s.jobs {
		for _, tag := range tags {
			if slices.Contains(j.tags, tag) {
				j.stop()
				delete(s.jobs, j.id)
				break
			}
		}
	}
}

func (s *scheduler) selectStart() {
	s.logger.Debug("gocron: 调度器启动")
	go s.exec.start()

	s.started = true
	for id, j := range s.jobs {
		next := j.startTime
		if j.startImmediately {
			next = s.now()
			select {
			case <-s.shutdownCtx.Done():
			case s.exec.jobsIn <- jobIn{
				id:            id,
				shouldSendOut: true,
			}:
			}
		} else {
			if next.IsZero() {
				next = j.next(s.now())
			}

			jobID := id
			j.timer = s.exec.clock.AfterFunc(next.Sub(s.now()), func() {
				select {
				case <-s.shutdownCtx.Done():
				case s.exec.jobsIn <- jobIn{
					id:            jobID,
					shouldSendOut: true,
				}:
				}
			})
		}
		j.startTime = next
		j.nextScheduled = append(j.nextScheduled, next)
		s.jobs[id] = j
	}
	select {
	case <-s.shutdownCtx.Done():
	case s.startedCh <- struct{}{}:
		s.logger.Info("gocron: 调度器已启动")
	}
}

// -----------------------------------------------
// -----------------------------------------------
// ------------- Scheduler 方法 ---------------
// -----------------------------------------------
// -----------------------------------------------

func (s *scheduler) now() time.Time {
	return s.exec.clock.Now().In(s.location)
}

func (s *scheduler) jobFromInternalJob(in internalJob) job {
	return job{
		in.id,
		in.name,
		slices.Clone(in.tags),
		s.jobOutRequestCh,
		s.runJobRequestCh,
	}
}

func (s *scheduler) Jobs() []Job {
	outChan := make(chan []Job)
	select {
	case <-s.shutdownCtx.Done():
	case s.allJobsOutRequest <- allJobsOutRequest{outChan: outChan}:
	}

	var jobs []Job
	select {
	case <-s.shutdownCtx.Done():
	case jobs = <-outChan:
	}

	return jobs
}

func (s *scheduler) NewJob(jobDefinition JobDefinition, task Task, options ...JobOption) (Job, error) {
	return s.addOrUpdateJob(uuid.Nil, jobDefinition, task, options)
}

func (s *scheduler) verifyInterfaceVariadic(taskFunc reflect.Value, tsk task, variadicStart int) error {
	ifaceType := taskFunc.Type().In(variadicStart).Elem()
	for i := variadicStart; i < len(tsk.parameters); i++ {
		if !reflect.TypeOf(tsk.parameters[i]).Implements(ifaceType) {
			return ErrNewJobWrongTypeOfParameters
		}
	}
	return nil
}

func (s *scheduler) verifyVariadic(taskFunc reflect.Value, tsk task, variadicStart int) error {
	if err := s.verifyNonVariadic(taskFunc, tsk, variadicStart); err != nil {
		return err
	}
	parameterType := taskFunc.Type().In(variadicStart).Elem().Kind()
	if parameterType == reflect.Interface {
		return s.verifyInterfaceVariadic(taskFunc, tsk, variadicStart)
	}
	if parameterType == reflect.Pointer {
		parameterType = reflect.Indirect(reflect.ValueOf(taskFunc.Type().In(variadicStart))).Kind()
	}

	for i := variadicStart; i < len(tsk.parameters); i++ {
		argumentType := reflect.TypeOf(tsk.parameters[i]).Kind()
		if argumentType == reflect.Interface || argumentType == reflect.Pointer {
			argumentType = reflect.TypeOf(tsk.parameters[i]).Elem().Kind()
		}
		if argumentType != parameterType {
			return ErrNewJobWrongTypeOfParameters
		}
	}
	return nil
}

func (s *scheduler) verifyNonVariadic(taskFunc reflect.Value, tsk task, length int) error {
	for i := 0; i < length; i++ {
		t1 := reflect.TypeOf(tsk.parameters[i]).Kind()
		if t1 == reflect.Interface || t1 == reflect.Pointer {
			t1 = reflect.TypeOf(tsk.parameters[i]).Elem().Kind()
		}
		t2 := reflect.New(taskFunc.Type().In(i)).Elem().Kind()
		if t2 == reflect.Interface || t2 == reflect.Pointer {
			t2 = reflect.Indirect(reflect.ValueOf(taskFunc.Type().In(i))).Kind()
		}
		if t1 != t2 {
			return ErrNewJobWrongTypeOfParameters
		}
	}
	return nil
}

func (s *scheduler) verifyParameterType(taskFunc reflect.Value, tsk task) error {
	isVariadic := taskFunc.Type().IsVariadic()
	if isVariadic {
		variadicStart := taskFunc.Type().NumIn() - 1
		return s.verifyVariadic(taskFunc, tsk, variadicStart)
	}
	expectedParameterLength := taskFunc.Type().NumIn()
	if len(tsk.parameters) != expectedParameterLength {
		return ErrNewJobWrongNumberOfParameters
	}
	return s.verifyNonVariadic(taskFunc, tsk, expectedParameterLength)
}

func (s *scheduler) addOrUpdateJob(id uuid.UUID, definition JobDefinition, taskWrapper Task, options []JobOption) (Job, error) {
	j := internalJob{}
	if id == uuid.Nil {
		j.id = uuid.New()
	} else {
		currentJob := requestJobCtx(s.shutdownCtx, id, s.jobOutRequestCh)
		if currentJob != nil && currentJob.id != uuid.Nil {
			select {
			case <-s.shutdownCtx.Done():
				return nil, nil
			case s.removeJobCh <- id:
				<-currentJob.ctx.Done()
			}
		}

		j.id = id
	}

	j.ctx, j.cancel = context.WithCancel(s.shutdownCtx)

	if taskWrapper == nil {
		return nil, ErrNewJobTaskNil
	}

	tsk := taskWrapper()
	taskFunc := reflect.ValueOf(tsk.function)
	for taskFunc.Kind() == reflect.Ptr {
		taskFunc = taskFunc.Elem()
	}

	if taskFunc.Kind() != reflect.Func {
		return nil, ErrNewJobTaskNotFunc
	}

	if err := s.verifyParameterType(taskFunc, tsk); err != nil {
		return nil, err
	}

	j.name = runtime.FuncForPC(taskFunc.Pointer()).Name()
	j.function = tsk.function
	j.parameters = tsk.parameters

	// 应用全局选项
	for _, option := range s.globalJobOptions {
		if err := option(&j, s.now()); err != nil {
			return nil, err
		}
	}

	// 应用工作特定选项，这些选项优先
	for _, option := range options {
		if err := option(&j, s.now()); err != nil {
			return nil, err
		}
	}

	if err := definition.setup(&j, s.location, s.exec.clock.Now()); err != nil {
		return nil, err
	}

	newJobCtx, newJobCancel := context.WithCancel(context.Background())
	select {
	case <-s.shutdownCtx.Done():
	case s.newJobCh <- newJobIn{
		ctx:    newJobCtx,
		cancel: newJobCancel,
		job:    j,
	}:
	}

	select {
	case <-newJobCtx.Done():
	case <-s.shutdownCtx.Done():
	}

	out := s.jobFromInternalJob(j)
	return &out, nil
}

func (s *scheduler) RemoveByTags(tags ...string) {
	select {
	case <-s.shutdownCtx.Done():
	case s.removeJobsByTagsCh <- tags:
	}
}

func (s *scheduler) RemoveJob(id uuid.UUID) error {
	j := requestJobCtx(s.shutdownCtx, id, s.jobOutRequestCh)
	if j == nil || j.id == uuid.Nil {
		return ErrJobNotFound
	}
	select {
	case <-s.shutdownCtx.Done():
	case s.removeJobCh <- id:
	}

	return nil
}

func (s *scheduler) Start() {
	select {
	case <-s.shutdownCtx.Done():
	case s.startCh <- struct{}{}:
		<-s.startedCh
	}
}

func (s *scheduler) StopJobs() error {
	select {
	case <-s.shutdownCtx.Done():
		return nil
	case s.stopCh <- struct{}{}:
	}
	select {
	case err := <-s.stopErrCh:
		return err
	case <-time.After(s.exec.stopTimeout + 2*time.Second):
		return ErrStopSchedulerTimedOut
	}
}

func (s *scheduler) Shutdown() error {
	s.shutdownCancel()
	select {
	case err := <-s.stopErrCh:
		return err
	case <-time.After(s.exec.stopTimeout + 2*time.Second):
		return ErrStopSchedulerTimedOut
	}
}

func (s *scheduler) Update(id uuid.UUID, jobDefinition JobDefinition, task Task, options ...JobOption) (Job, error) {
	return s.addOrUpdateJob(id, jobDefinition, task, options)
}

func (s *scheduler) JobsWaitingInQueue() int {
	if s.exec.limitMode != nil && s.exec.limitMode.mode == LimitModeWait {
		return len(s.exec.limitMode.in)
	}
	return 0
}

// -----------------------------------------------
// -----------------------------------------------
// ------------- Scheduler Options ---------------
// -----------------------------------------------
// -----------------------------------------------

// SchedulerOption 定义在 Scheduler 上设置选项的函数。
type SchedulerOption func(*scheduler) error

// WithClock 将 Scheduler 使用的 clock 设置为所提供的 clock
// 更多信息看 https://github.com/jonboulle/clockwork
func WithClock(clock clockwork.Clock) SchedulerOption {
	return func(s *scheduler) error {
		if clock == nil {
			return ErrWithClockNil
		}
		s.exec.clock = clock
		return nil
	}
}

// WithDistributedElector WithDistributedElector 设置多个 Scheduler 实例要使用的 elector，
// 以确定谁应该成为领导者。只有领导者运行作业，而非领导者等待并继续检查是否已选出新的领导者。
func WithDistributedElector(elector Elector) SchedulerOption {
	return func(s *scheduler) error {
		if elector == nil {
			return ErrWithDistributedElectorNil
		}
		s.exec.elector = elector
		return nil
	}
}

// WithDistributedLocker 设置多个 Locker
// Scheduler 来确保每个作业只运行一个实例。
func WithDistributedLocker(locker Locker) SchedulerOption {
	return func(s *scheduler) error {
		if locker == nil {
			return ErrWithDistributedLockerNil
		}
		s.exec.locker = locker
		return nil
	}
}

// WithGlobalJobOptions 设置 JobOption，该选项将应用于添加到调度程序的所有作业。
// 如果全局设置了相同的 JobOption，则在作业本身上设置的 JobOption 将被覆盖。
func WithGlobalJobOptions(jobOptions ...JobOption) SchedulerOption {
	return func(s *scheduler) error {
		s.globalJobOptions = jobOptions
		return nil
	}
}

// LimitMode 定义用于处理达到 WithLimitConcurrentJobs 中提供的限制
type LimitMode int

const (
	// LimitModeReschedule 会导致跳过达到 WithLimitConcurrentJobs
	// 或 WithSingletonMode 中设置的限制的作业，并将其重新安排在下一次运行时运行，而不是排队等待。
	LimitModeReschedule = 1

	// LimitModeWait 导致作业到达WithLimitConcurrentJobs或WithSingletonMode设置的限制，
	// 在队列中等待，直到有可用的任务槽可以运行。
	// 注意:这种模式可能会产生不可预测的结果，因为作业的执行顺序无法保证。
	// 例如，一个频繁执行的作业可能会堆积在等待队列中，在队列打开时被往后拖延很多次后执行。
	// 警告:如果你的作业会持续堆积，超出极限worker的能力，请不要使用这种模式。
	// 一个不能使用的例子:
	//
	//     s, _ := gocron.NewScheduler(gocron.WithLimitConcurrentJobs)
	//     s.NewJob(
	//         gocron.DurationJob(
	//				time.Second,
	//				Task{
	//					Function: func() {
	//						time.Sleep(10 * time.Second)
	//					},
	//				},
	//			),
	//      )
	LimitModeWait = 2
)

// WithLimitConcurrentJobs 设置调度器用于限制在给定时间内可能运行的作业数量的限制和模式。
//
// 注意:如果您同时使用WithSingletonMode在作业级别运行限制模式，
// 则WithLimitConcurrentJobs选择的限制模式具有初始优先级。
//
// 警告:当你同时运行limitconcurrentjobs (1, LimitModeWait)的调度器限制
// 和singletonmode (limitmoderesschedule)的作业限制时，单个耗时的作业可能会支配你的限制。
func WithLimitConcurrentJobs(limit uint, mode LimitMode) SchedulerOption {
	return func(s *scheduler) error {
		if limit == 0 {
			return ErrWithLimitConcurrentJobsZero
		}
		s.exec.limitMode = &limitModeConfig{
			mode:          mode,
			limit:         limit,
			in:            make(chan jobIn, 1000),
			singletonJobs: make(map[uuid.UUID]struct{}),
		}
		if mode == LimitModeReschedule {
			s.exec.limitMode.rescheduleLimiter = make(chan struct{}, limit)
		}
		return nil
	}
}

// WithLocation 设置调度器应该操作的位置(即时区)。在许多系统时间。Local是UTC。
// Default: time.Local
func WithLocation(location *time.Location) SchedulerOption {
	return func(s *scheduler) error {
		if location == nil {
			return ErrWithLocationNil
		}
		s.location = location
		return nil
	}
}

// WithLogger 设置调度器使用的logger。
func WithLogger(logger Logger) SchedulerOption {
	return func(s *scheduler) error {
		if logger == nil {
			return ErrWithLoggerNil
		}
		s.logger = logger
		s.exec.logger = logger
		return nil
	}
}

// WithStopTimeout 设置当调用StopJobs()或Shutdown()时，
// 调度器应该优雅地等待作业完成的时间，然后返回。
// Default: 10 * time.Second
func WithStopTimeout(timeout time.Duration) SchedulerOption {
	return func(s *scheduler) error {
		if timeout <= 0 {
			return ErrWithStopTimeoutZeroOrNegative
		}
		s.exec.stopTimeout = timeout
		return nil
	}
}

// WithMonitor 设置 Scheduler 要使用的指标提供程序。
func WithMonitor(monitor Monitor) SchedulerOption {
	return func(s *scheduler) error {
		if monitor == nil {
			return ErrWithMonitorNil
		}
		s.exec.monitor = monitor
		return nil
	}
}
