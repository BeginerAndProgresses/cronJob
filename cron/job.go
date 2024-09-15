//go:generate mockgen -destination=mocks/job.go -package=gocronmocks . Job
package cron

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/robfig/cron/v3"
	"golang.org/x/exp/slices"
)

// internalJob 存储调度器所需的信息
// 包括管理调度、启动和停止作业
type internalJob struct {
	ctx    context.Context
	cancel context.CancelFunc
	id     uuid.UUID
	name   string
	tags   []string
	jobSchedule

	//因为有些作业可能会排队，所以可以这样做
	//有多个 nextScheduled 的时间存在
	nextScheduled []time.Time

	lastRun            time.Time
	function           any
	parameters         []any
	timer              clockwork.Timer
	singletonMode      bool
	singletonLimitMode LimitMode
	limitRunsTo        *limitRunsTo
	startTime          time.Time
	startImmediately   bool
	stopTime           time.Time
	// 事件监听器
	afterJobRuns          func(jobID uuid.UUID, jobName string)
	beforeJobRuns         func(jobID uuid.UUID, jobName string)
	afterJobRunsWithError func(jobID uuid.UUID, jobName string, err error)
	afterJobRunsWithPanic func(jobID uuid.UUID, jobName string, recoverData any)
	afterLockError        func(jobID uuid.UUID, jobName string, err error)

	locker Locker
}

// Stop用于停止作业的定时器并取消上下文。
// 停止定时器对于清理处于睡眠状态的作业至关重要。
// 当作业被停止时，AfterFunc定时器。
// 取消上下文会阻止执行器继续尝试运行作业。
func (j *internalJob) stop() {
	if j.timer != nil {
		j.timer.Stop()
	}
	j.cancel()
}

func (j *internalJob) stopTimeReached(now time.Time) bool {
	if j.stopTime.IsZero() {
		return false
	}
	return j.stopTime.Before(now)
}

// task 存储作业执行时实际运行的函数和参数。
type task struct {
	function   any
	parameters []any
}

// Task defines 返回一个task，包含一个函数和参数
type Task func() task

// NewTask 提供作业的任务函数和参数。
func NewTask(function any, parameters ...any) Task {
	return func() task {
		return task{
			function:   function,
			parameters: parameters,
		}
	}
}

// limitRunsTo 用于管理运行次数
// 当用户只希望作业运行一定的
// 运行次数，然后从调度程序中删除。
type limitRunsTo struct {
	limit    uint
	runCount uint
}

// -----------------------------------------------
// -----------------------------------------------
// --------------- Job 变体 ------------------
// -----------------------------------------------
// -----------------------------------------------

// JobDefinition 定义了一个必须实现根据定义创建作业的接口。
type JobDefinition interface {
	setup(j *internalJob, l *time.Location, now time.Time) error
}

var _ JobDefinition = (*cronJobDefinition)(nil)

type cronJobDefinition struct {
	crontab     string
	withSeconds bool
}

func (c cronJobDefinition) setup(j *internalJob, location *time.Location, now time.Time) error {
	var withLocation string
	if strings.HasPrefix(c.crontab, "TZ=") || strings.HasPrefix(c.crontab, "CRON_TZ=") {
		withLocation = c.crontab
	} else {
		// 因为用户没有为该位置提供默认时区
		// 由调度器传入。默认值:time.Location
		withLocation = fmt.Sprintf("CRON_TZ=%s %s", location.String(), c.crontab)
	}

	var (
		cronSchedule cron.Schedule
		err          error
	)

	if c.withSeconds {
		p := cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
		cronSchedule, err = p.Parse(withLocation)
	} else {
		cronSchedule, err = cron.ParseStandard(withLocation)
	}
	if err != nil {
		return errors.Join(ErrCronJobParse, err)
	}
	if cronSchedule.Next(now).IsZero() {
		return ErrCronJobInvalid
	}

	j.jobSchedule = &cronJob{cronSchedule: cronSchedule}
	return nil
}

// CronJob 使用crontab语法定义一个新作业:`* * * * *`。
// 如果withSeconds设置为true，可选的第6个字段可以在开头使用:`* * * * * *`。
// 时区可以使用WithLocation在Scheduler上设置，也可以在crontab中以' TZ=AmericaChicago * * * * *'
// 或' CRON_TZ=AmericaChicago * * * * *'的形式设置。
func CronJob(crontab string, withSeconds bool) JobDefinition {
	return cronJobDefinition{
		crontab:     crontab,
		withSeconds: withSeconds,
	}
}

var _ JobDefinition = (*durationJobDefinition)(nil)

type durationJobDefinition struct {
	duration time.Duration
}

func (d durationJobDefinition) setup(j *internalJob, _ *time.Location, _ time.Time) error {
	if d.duration == 0 {
		return ErrDurationJobIntervalZero
	}
	j.jobSchedule = &durationJob{duration: d.duration}
	return nil
}

// DurationJob 定义一个新的作业使用time.Duration作为间隔时间
func DurationJob(duration time.Duration) JobDefinition {
	return durationJobDefinition{
		duration: duration,
	}
}

var _ JobDefinition = (*durationRandomJobDefinition)(nil)

type durationRandomJobDefinition struct {
	min, max time.Duration
}

func (d durationRandomJobDefinition) setup(j *internalJob, _ *time.Location, _ time.Time) error {
	if d.min >= d.max {
		return ErrDurationRandomJobMinMax
	}

	j.jobSchedule = &durationRandomJob{
		min:  d.min,
		max:  d.max,
		rand: rand.New(rand.NewSource(time.Now().UnixNano())), // nolint:gosec
	}
	return nil
}

// DurationRandomJob 定义一个新作业，该作业在所提供的最小和最大持续时间值之间的随机间隔内运行。
// 在提供的最小和最大持续时间值之间。
//
// 为了实现与使用延展/抖动技术的工具类似的行为
// 将中值视为基线，将最大值-中值或中值-最小值之间的差值视为最大值-中值或中值-最小值之间的差值。
// 最大-中值或中值-最小值之间的差值作为延展/抖动值。
//
// 例如，如果您希望作业每 5 分钟运行一次，但又想增加
// 最多 1 分钟的抖动，可以使用
// DurationRandomJob(4*time.Minute, 6*time.Minute)
func DurationRandomJob(minDuration, maxDuration time.Duration) JobDefinition {
	return durationRandomJobDefinition{
		min: minDuration,
		max: maxDuration,
	}
}

// DailyJob 在设定的时间间隔内运行作业。
// 默认情况下，作业将在下一个可用日开始，并将上次运行时间视为现在、
// 并根据您输入的时间间隔和时间来确定时间和日期。这意味着，如果您
// 选择的时间间隔大于 1，则作业默认将从现在开始运行 X（时间间隔）天。
// 如果当前天没有剩余的 atTimes。您可以使用 WithStartAt 告诉
// 调度程序提前启动作业。
func DailyJob(interval uint, atTimes AtTimes) JobDefinition {
	return dailyJobDefinition{
		interval: interval,
		atTimes:  atTimes,
	}
}

var _ JobDefinition = (*dailyJobDefinition)(nil)

type dailyJobDefinition struct {
	interval uint
	atTimes  AtTimes
}

func (d dailyJobDefinition) setup(j *internalJob, location *time.Location, _ time.Time) error {
	atTimesDate, err := convertAtTimesToDateTime(d.atTimes, location)
	switch {
	case errors.Is(err, errAtTimesNil):
		return ErrDailyJobAtTimesNil
	case errors.Is(err, errAtTimeNil):
		return ErrDailyJobAtTimeNil
	case errors.Is(err, errAtTimeHours):
		return ErrDailyJobHours
	case errors.Is(err, errAtTimeMinSec):
		return ErrDailyJobMinutesSeconds
	}

	ds := dailyJob{
		interval: d.interval,
		atTimes:  atTimesDate,
	}
	j.jobSchedule = ds
	return nil
}

var _ JobDefinition = (*weeklyJobDefinition)(nil)

type weeklyJobDefinition struct {
	interval      uint
	daysOfTheWeek Weekdays
	atTimes       AtTimes
}

func (w weeklyJobDefinition) setup(j *internalJob, location *time.Location, _ time.Time) error {
	var ws weeklyJob
	ws.interval = w.interval

	if w.daysOfTheWeek == nil {
		return ErrWeeklyJobDaysOfTheWeekNil
	}

	daysOfTheWeek := w.daysOfTheWeek()

	slices.Sort(daysOfTheWeek)
	ws.daysOfWeek = daysOfTheWeek

	atTimesDate, err := convertAtTimesToDateTime(w.atTimes, location)
	switch {
	case errors.Is(err, errAtTimesNil):
		return ErrWeeklyJobAtTimesNil
	case errors.Is(err, errAtTimeNil):
		return ErrWeeklyJobAtTimeNil
	case errors.Is(err, errAtTimeHours):
		return ErrWeeklyJobHours
	case errors.Is(err, errAtTimeMinSec):
		return ErrWeeklyJobMinutesSeconds
	}
	ws.atTimes = atTimesDate

	j.jobSchedule = ws
	return nil
}

// Weekdays 定义了一个返回星期列表的函数。
type Weekdays func() []time.Weekday

// NewWeekdays 提供作业应该运行的星期几。
func NewWeekdays(weekday time.Weekday, weekdays ...time.Weekday) Weekdays {
	return func() []time.Weekday {
		weekdays = append(weekdays, weekday)
		return weekdays
	}
}

// WeeklyJob 在指定的星期、特定的日期和时间运行作业。
// 指定的日期和设定的时间运行作业。
//
// 默认情况下，作业将在下一个可用日开始，并将上次运行时间视为现在、
// 并根据您输入的时间间隔、天数和时间确定时间和日期。这意味着，如果您
// 选择的时间间隔大于 1，则作业默认将从现在开始运行 X（时间间隔）周
// 如果当前周内没有剩余的设定天数。您可以使用 WithStartAt 告诉
// 调度程序提前启动作业。
func WeeklyJob(interval uint, daysOfTheWeek Weekdays, atTimes AtTimes) JobDefinition {
	return weeklyJobDefinition{
		interval:      interval,
		daysOfTheWeek: daysOfTheWeek,
		atTimes:       atTimes,
	}
}

var _ JobDefinition = (*monthlyJobDefinition)(nil)

type monthlyJobDefinition struct {
	interval       uint
	daysOfTheMonth DaysOfTheMonth
	atTimes        AtTimes
}

func (m monthlyJobDefinition) setup(j *internalJob, location *time.Location, _ time.Time) error {
	var ms monthlyJob
	ms.interval = m.interval

	if m.daysOfTheMonth == nil {
		return ErrMonthlyJobDaysNil
	}

	var daysStart, daysEnd []int
	for _, day := range m.daysOfTheMonth() {
		if day > 31 || day == 0 || day < -31 {
			return ErrMonthlyJobDays
		}
		if day > 0 {
			daysStart = append(daysStart, day)
		} else {
			daysEnd = append(daysEnd, day)
		}
	}
	daysStart = removeSliceDuplicatesInt(daysStart)
	slices.Sort(daysStart)
	ms.days = daysStart

	daysEnd = removeSliceDuplicatesInt(daysEnd)
	slices.Sort(daysEnd)
	ms.daysFromEnd = daysEnd

	atTimesDate, err := convertAtTimesToDateTime(m.atTimes, location)
	switch {
	case errors.Is(err, errAtTimesNil):
		return ErrMonthlyJobAtTimesNil
	case errors.Is(err, errAtTimeNil):
		return ErrMonthlyJobAtTimeNil
	case errors.Is(err, errAtTimeHours):
		return ErrMonthlyJobHours
	case errors.Is(err, errAtTimeMinSec):
		return ErrMonthlyJobMinutesSeconds
	}
	ms.atTimes = atTimesDate

	j.jobSchedule = ms
	return nil
}

type days []int

// DaysOfTheMonth 定义一个返回天数列表的函数。
type DaysOfTheMonth func() days

// NewDaysOfTheMonth 提供了任务在本月应
// 运行的天数可以是正数 1 至 31 天和/或负数 -31 至 -1 天。
// 负值从月末开始倒数。
// 例如：-1 == 本月最后一天。
//
// -5 == 月底前 5 天。
func NewDaysOfTheMonth(day int, moreDays ...int) DaysOfTheMonth {
	return func() days {
		moreDays = append(moreDays, day)
		return moreDays
	}
}

type atTime struct {
	hours, minutes, seconds uint
}

func (a atTime) time(location *time.Location) time.Time {
	return time.Date(0, 0, 0, int(a.hours), int(a.minutes), int(a.seconds), 0, location)
}

// AtTime 定义了一个返回内部 atTime 的函数
type AtTime func() atTime

// NewAtTime 提供运行任务的时、分、秒。
func NewAtTime(hours, minutes, seconds uint) AtTime {
	return func() atTime {
		return atTime{hours: hours, minutes: minutes, seconds: seconds}
	}
}

// AtTimes 定义一个 AtTime 的列表
type AtTimes func() []AtTime

// NewAtTimes 提供运行任务的时、分、秒。
func NewAtTimes(atTime AtTime, atTimes ...AtTime) AtTimes {
	return func() []AtTime {
		atTimes = append(atTimes, atTime)
		return atTimes
	}
}

// MonthlyJob 以月为间隔，在每月的特定日期运行作业。
// 指定的日期和时间运行作业。每月的天数可以是 1 到 31 天，也可以是负数（-1 到 -31），从月末开始倒数。
// 从月底开始倒数。例如，-1 表示每月的最后一天。
//
// 如果选择的月份中的某一天在所有月份中都不存在（例如 31 号）
// 没有这一天的月份将被跳过。
//
// 默认情况下，工作将在下一个可用日期开始，并将上次运行时间视为现在、
// 并根据您输入的时间间隔、天数和时间来确定时间和月份。
// 这意味着，如果您选择的时间间隔大于 1，则作业默认将从现在开始运行
// 如果当前月份没有剩余的月份天数，则作业将从现在开始运行 X（间隔）个月。
// 您可以使用 WithStartAt 命令调度程序提前启动作业。
//
// 仔细考虑您的配置！
// 例如：从 12/31 开始，每月 31 日为 2 个月的时间间隔。
// 将跳过 2 月、4 月、6 月，下一次运行将在 8 月。
func MonthlyJob(interval uint, daysOfTheMonth DaysOfTheMonth, atTimes AtTimes) JobDefinition {
	return monthlyJobDefinition{
		interval:       interval,
		daysOfTheMonth: daysOfTheMonth,
		atTimes:        atTimes,
	}
}

var _ JobDefinition = (*oneTimeJobDefinition)(nil)

type oneTimeJobDefinition struct {
	startAt OneTimeJobStartAtOption
}

func (o oneTimeJobDefinition) setup(j *internalJob, _ *time.Location, now time.Time) error {
	sortedTimes := o.startAt(j)
	slices.SortStableFunc(sortedTimes, ascendingTime)
	// 只保存将来的日程表
	idx, found := slices.BinarySearchFunc(sortedTimes, now, ascendingTime)
	if found {
		idx++
	}
	sortedTimes = sortedTimes[idx:]
	if !j.startImmediately && len(sortedTimes) == 0 {
		return ErrOneTimeJobStartDateTimePast
	}
	j.jobSchedule = oneTimeJob{sortedTimes: sortedTimes}
	return nil
}

// OneTimeJobStartAtOption 定义一次性作业何时运行
type OneTimeJobStartAtOption func(*internalJob) []time.Time

// OneTimeJobStartImmediately 告诉调度器立即运行一次性作业。
func OneTimeJobStartImmediately() OneTimeJobStartAtOption {
	return func(j *internalJob) []time.Time {
		j.startImmediately = true
		return []time.Time{}
	}
}

// OneTimeJobStartDateTime 设置作业应该运行的日期和时间。
// 这个日期时间必须在未来(根据调度器的时钟)
func OneTimeJobStartDateTime(start time.Time) OneTimeJobStartAtOption {
	return func(_ *internalJob) []time.Time {
		return []time.Time{start}
	}
}

// OneTimeJobStartDateTimes 设置作业运行的日期和时间。
// 至少有一个日期/时间必须在未来(根据调度器的时钟)
func OneTimeJobStartDateTimes(times ...time.Time) OneTimeJobStartAtOption {
	return func(_ *internalJob) []time.Time {
		return times
	}
}

// OneTimeJob 是在指定时间运行一次作业，而不是按照任何定期计划执行。
func OneTimeJob(startAt OneTimeJobStartAtOption) JobDefinition {
	return oneTimeJobDefinition{
		startAt: startAt,
	}
}

// -----------------------------------------------
// -----------------------------------------------
// ----------------- Job 配置 -----------------
// -----------------------------------------------
// -----------------------------------------------

// JobOption 定义作业配置的构造函数。
type JobOption func(*internalJob, time.Time) error

// WithDistributedJobLocker （分布式任务锁定器）将锁定器设置为由多个
// 调度程序实例使用的锁定器，以确保每个实例同一时间是有一个调度器在允许
func WithDistributedJobLocker(locker Locker) JobOption {
	return func(j *internalJob, _ time.Time) error {
		if locker == nil {
			return ErrWithDistributedJobLockerNil
		}
		j.locker = locker
		return nil
	}
}

// WithEventListeners 设置应为任务运行的事件监听器。
func WithEventListeners(eventListeners ...EventListener) JobOption {
	return func(j *internalJob, _ time.Time) error {
		for _, eventListener := range eventListeners {
			if err := eventListener(j); err != nil {
				return err
			}
		}
		return nil
	}
}

// WithLimitedRuns 将该作业的执行次数限制为 n。
// 达到限制后，作业将从调度程序中移除。
func WithLimitedRuns(limit uint) JobOption {
	return func(j *internalJob, _ time.Time) error {
		j.limitRunsTo = &limitRunsTo{
			limit:    limit,
			runCount: 0,
		}
		return nil
	}
}

// WithName 设置任务名称。使得可读性更高。
func WithName(name string) JobOption {
	return func(j *internalJob, _ time.Time) error {
		if name == "" {
			return ErrWithNameEmpty
		}
		j.name = name
		return nil
	}
}

// WithSingletonMode 使已经运行的作业不再运行。
// 这对于不应重叠的作业以及偶尔
// 运行时间超过作业运行间隔时间的作业来说非常有用。
func WithSingletonMode(mode LimitMode) JobOption {
	return func(j *internalJob, _ time.Time) error {
		j.singletonMode = true
		j.singletonLimitMode = mode
		return nil
	}
}

// WithStartAt 设置在特定日期启动任务的选项。
func WithStartAt(option StartAtOption) JobOption {
	return func(j *internalJob, now time.Time) error {
		return option(j, now)
	}
}

// StartAtOption defines options for starting the job
type StartAtOption func(*internalJob, time.Time) error

// WithStartImmediately 命令调度程序立即运行作业。
// 无论作业类型或计划如何。立即运行后
// 作业将从此时开始根据作业定义进行调度。
func WithStartImmediately() StartAtOption {
	return func(j *internalJob, _ time.Time) error {
		j.startImmediately = true
		return nil
	}
}

// WithStartDateTime 设置任务运行的第一个日期和时间。
// 这个日期时间必须在未来。
func WithStartDateTime(start time.Time) StartAtOption {
	return func(j *internalJob, now time.Time) error {
		if start.IsZero() || start.Before(now) {
			return ErrWithStartDateTimePast
		}
		if !j.stopTime.IsZero() && j.stopTime.Before(start) {
			return ErrStartTimeLaterThanEndTime
		}
		j.startTime = start
		return nil
	}
}

// WithStopAt 设置在指定时间后停止运行作业的选项。
func WithStopAt(option StopAtOption) JobOption {
	return func(j *internalJob, now time.Time) error {
		return option(j, now)
	}
}

// StopAtOption 定义了停止工作的选项
type StopAtOption func(*internalJob, time.Time) error

// WithStopDateTime 设置作业停止的最终日期和时间。
// 这必须是未来的日期和时间，并且应在开始时间（如果已指定）之后。
// 作业的最终运行时间可以是停止时间，但不能在停止时间之后。
func WithStopDateTime(end time.Time) StopAtOption {
	return func(j *internalJob, now time.Time) error {
		if end.IsZero() || end.Before(now) {
			return ErrWithStopDateTimePast
		}
		if end.Before(j.startTime) {
			return ErrStopTimeEarlierThanStartTime
		}
		j.stopTime = end
		return nil
	}
}

// WithTags 设置任务的标记。
// 标签提供了一种通过一组标签识别作业和删除多个任务方式。
func WithTags(tags ...string) JobOption {
	return func(j *internalJob, _ time.Time) error {
		j.tags = tags
		return nil
	}
}

// WithIdentifier 设置作业的标识符。标识符
// 用于唯一标识作业并用于记录日志和指标。
func WithIdentifier(id uuid.UUID) JobOption {
	return func(j *internalJob, _ time.Time) error {
		if id == uuid.Nil {
			return ErrWithIdentifierNil
		}

		j.id = id
		return nil
	}
}

// -----------------------------------------------
// -----------------------------------------------
// ------------- Job 事件监听 -------------
// -----------------------------------------------
// -----------------------------------------------

// EventListener 定义了事件监听器的构造函数。
// 事件监听器的构造函数。
type EventListener func(*internalJob) error

// BeforeJobRuns 用于侦听作业即将运行的时间，然后运行所提供的函数。
// 然后运行所提供的函数。
func BeforeJobRuns(eventListenerFunc func(jobID uuid.UUID, jobName string)) EventListener {
	return func(j *internalJob) error {
		if eventListenerFunc == nil {
			return ErrEventListenerFuncNil
		}
		j.beforeJobRuns = eventListenerFunc
		return nil
	}
}

// AfterJobRuns 用于监听作业是否已无差错运行，然后运行所提供的函数。
// 无错误运行，然后运行所提供的函数。
func AfterJobRuns(eventListenerFunc func(jobID uuid.UUID, jobName string)) EventListener {
	return func(j *internalJob) error {
		if eventListenerFunc == nil {
			return ErrEventListenerFuncNil
		}
		j.afterJobRuns = eventListenerFunc
		return nil
	}
}

// AfterJobRunsWithError 用于侦听作业是否已运行并
// 返回错误，然后运行所提供的函数。
func AfterJobRunsWithError(eventListenerFunc func(jobID uuid.UUID, jobName string, err error)) EventListener {
	return func(j *internalJob) error {
		if eventListenerFunc == nil {
			return ErrEventListenerFuncNil
		}
		j.afterJobRunsWithError = eventListenerFunc
		return nil
	}
}

// AfterJobRunsWithPanic 用于监听作业运行并
// 返回panic的恢复数据，然后运行所提供的函数。
func AfterJobRunsWithPanic(eventListenerFunc func(jobID uuid.UUID, jobName string, recoverData any)) EventListener {
	return func(j *internalJob) error {
		if eventListenerFunc == nil {
			return ErrEventListenerFuncNil
		}
		j.afterJobRunsWithPanic = eventListenerFunc
		return nil
	}
}

// AfterLockError 用于当分布式锁返回错误时，运行所提供的函数。
func AfterLockError(eventListenerFunc func(jobID uuid.UUID, jobName string, err error)) EventListener {
	return func(j *internalJob) error {
		if eventListenerFunc == nil {
			return ErrEventListenerFuncNil
		}
		j.afterLockError = eventListenerFunc
		return nil
	}
}

// -----------------------------------------------
// -----------------------------------------------
// ---------------- Job 调度表  -------------------
// -----------------------------------------------
// -----------------------------------------------

type jobSchedule interface {
	next(lastRun time.Time) time.Time
}

var _ jobSchedule = (*cronJob)(nil)

type cronJob struct {
	cronSchedule cron.Schedule
}

func (j *cronJob) next(lastRun time.Time) time.Time {
	return j.cronSchedule.Next(lastRun)
}

var _ jobSchedule = (*durationJob)(nil)

type durationJob struct {
	duration time.Duration
}

func (j *durationJob) next(lastRun time.Time) time.Time {
	return lastRun.Add(j.duration)
}

var _ jobSchedule = (*durationRandomJob)(nil)

type durationRandomJob struct {
	min, max time.Duration
	rand     *rand.Rand
}

func (j *durationRandomJob) next(lastRun time.Time) time.Time {
	r := j.rand.Int63n(int64(j.max - j.min))
	return lastRun.Add(j.min + time.Duration(r))
}

var _ jobSchedule = (*dailyJob)(nil)

type dailyJob struct {
	interval uint
	atTimes  []time.Time
}

func (d dailyJob) next(lastRun time.Time) time.Time {
	firstPass := true
	next := d.nextDay(lastRun, firstPass)
	if !next.IsZero() {
		return next
	}
	firstPass = false

	startNextDay := time.Date(lastRun.Year(), lastRun.Month(), lastRun.Day()+int(d.interval), 0, 0, 0, lastRun.Nanosecond(), lastRun.Location())
	return d.nextDay(startNextDay, firstPass)
}

func (d dailyJob) nextDay(lastRun time.Time, firstPass bool) time.Time {
	for _, at := range d.atTimes {
		// 在 lastScheduledRun 的值中加入时间（小时/分钟/秒）。
		// 用于检查是否有下一次运行时间
		atDate := time.Date(lastRun.Year(), lastRun.Month(), lastRun.Day(), at.Hour(), at.Minute(), at.Second(), lastRun.Nanosecond(), lastRun.Location())

		if firstPass && atDate.After(lastRun) {
			// 检查它是否在 "大于 "之后、
			// 而不是大于或等于我们的 lastScheduledRun 日/时间。
			// 将出现在循环中，我们不想再次选择它
			return atDate
		} else if !firstPass && !atDate.Before(lastRun) {
			// 现在我们要看的是第二天，因此可以考虑
			// 与上次运行时间相同（因为 lastScheduledRun 已递增）
			return atDate
		}
	}
	return time.Time{}
}

var _ jobSchedule = (*weeklyJob)(nil)

type weeklyJob struct {
	interval   uint
	daysOfWeek []time.Weekday
	atTimes    []time.Time
}

func (w weeklyJob) next(lastRun time.Time) time.Time {
	firstPass := true
	next := w.nextWeekDayAtTime(lastRun, firstPass)
	if !next.IsZero() {
		return next
	}
	firstPass = false

	startOfTheNextIntervalWeek := (lastRun.Day() - int(lastRun.Weekday())) + int(w.interval*7)
	from := time.Date(lastRun.Year(), lastRun.Month(), startOfTheNextIntervalWeek, 0, 0, 0, 0, lastRun.Location())
	return w.nextWeekDayAtTime(from, firstPass)
}

func (w weeklyJob) nextWeekDayAtTime(lastRun time.Time, firstPass bool) time.Time {
	for _, wd := range w.daysOfWeek {
		// 检查我们是否在同一天或同一星期的晚些时候
		if wd >= lastRun.Weekday() {
			// weekDayDiff 用于在下面的 atDate 日添加正确的数额
			weekDayDiff := wd - lastRun.Weekday()
			for _, at := range w.atTimes {
				// 在 lastScheduledRun 的值中加入时间（小时/分钟/秒）。
				// 用于检查是否有下一次运行时间
				atDate := time.Date(lastRun.Year(), lastRun.Month(), lastRun.Day()+int(weekDayDiff), at.Hour(), at.Minute(), at.Second(), lastRun.Nanosecond(), lastRun.Location())

				if firstPass && atDate.After(lastRun) {
					// 检查它是否在 "大于 "之后、
					// 而不是大于或等于我们的 lastScheduledRun 日/时间。
					// 将出现在循环中，我们不想再次选择它
					return atDate
				} else if !firstPass && !atDate.Before(lastRun) {
					// 既然我们正在考虑下一周，那么就可以考虑
					// 与上次运行时间相同（因为 lastScheduledRun 已递增）
					return atDate
				}
			}
		}
	}
	return time.Time{}
}

var _ jobSchedule = (*monthlyJob)(nil)

type monthlyJob struct {
	interval    uint
	days        []int
	daysFromEnd []int
	atTimes     []time.Time
}

func (m monthlyJob) next(lastRun time.Time) time.Time {
	daysList := make([]int, len(m.days))
	copy(daysList, m.days)

	daysFromEnd := m.handleNegativeDays(lastRun, daysList, m.daysFromEnd)
	next := m.nextMonthDayAtTime(lastRun, daysFromEnd, true)
	if !next.IsZero() {
		return next
	}

	from := time.Date(lastRun.Year(), lastRun.Month()+time.Month(m.interval), 1, 0, 0, 0, 0, lastRun.Location())
	for next.IsZero() {
		daysFromEnd = m.handleNegativeDays(from, daysList, m.daysFromEnd)
		next = m.nextMonthDayAtTime(from, daysFromEnd, false)
		from = from.AddDate(0, int(m.interval), 0)
	}

	return next
}

func (m monthlyJob) handleNegativeDays(from time.Time, days, negativeDays []int) []int {
	var out []int
	// 获取下一个月末的天数列表
	// -1 == 该月的最后一天
	firstDayNextMonth := time.Date(from.Year(), from.Month()+1, 1, 0, 0, 0, 0, from.Location())
	for _, daySub := range negativeDays {
		day := firstDayNextMonth.AddDate(0, 0, daySub).Day()
		out = append(out, day)
	}
	out = append(out, days...)
	slices.Sort(out)
	return out
}

func (m monthlyJob) nextMonthDayAtTime(lastRun time.Time, days []int, firstPass bool) time.Time {
	// 查找本月中应该运行的下一天，然后检查时间是否正确
	for _, day := range days {
		if day >= lastRun.Day() {
			for _, at := range m.atTimes {
				// 将日期和时间（小时/分钟/秒）代入 lastScheduledRun 的值中
				// 用于检查是否有下一次运行时间
				atDate := time.Date(lastRun.Year(), lastRun.Month(), day, at.Hour(), at.Minute(), at.Second(), lastRun.Nanosecond(), lastRun.Location())

				if atDate.Month() != lastRun.Month() {
					// 如果我们设置的日期不在当前月份，则进行此检查处理
					// 例如，将第 31 天设置为 2 月，结果就是 3 月 2 日
					continue
				}

				if firstPass && atDate.After(lastRun) {
					// 检查它是否在 "大于 "之后、
					// 而不是大于或等于我们的 lastScheduledRun 日/时间。
					// 将出现在循环中，我们不想再次选择它
					return atDate
				} else if !firstPass && !atDate.Before(lastRun) {
					// 现在我们要看下个月的情况，因此可以考虑
					// 与上次计划运行的时间相同（因为上次计划运行的时间已递增）
					return atDate
				}
			}
			continue
		}
	}
	return time.Time{}
}

var _ jobSchedule = (*oneTimeJob)(nil)

type oneTimeJob struct {
	sortedTimes []time.Time
}

// next 使用二元搜索，在时间排序列表中查找下一个项目。
//
// example: sortedTimes：[2, 4, 6, 8]
//
// lastRun: 1 => [idx=0,found=false] => next is 2 - sorted[idx] idx=0
// lastRun: 2 => [idx=0,found=true] => 下一个是 4 - 排序[idx+1] idx=1
// lastRun: 3 => [idx=1,found=false] => 下一个是 4 - 排序[idx] idx=1
// lastRun: 4 => [idx=1,found=true] => 下一个是 6 - 排序[idx+1] idx=2
// lastRun: 7 => [idx=3,found=false] => 下一个是 8 - 排序[idx] idx=3
// lastRun: 8 => [idx=3,found=found] => 下一个是无
// lastRun: 9 => [idx=3,found=found] => 下一个是无
func (o oneTimeJob) next(lastRun time.Time) time.Time {
	idx, found := slices.BinarySearchFunc(o.sortedTimes, lastRun, ascendingTime)
	// 如果找到，下一次运行将是以下索引
	if found {
		idx++
	}
	// 耗尽运行时间
	if idx >= len(o.sortedTimes) {
		return time.Time{}
	}

	return o.sortedTimes[idx]
}

// -----------------------------------------------
// -----------------------------------------------
// ---------------- Job 接口 ----------------
// -----------------------------------------------
// -----------------------------------------------

// Job 提供工作的可用方法
// 可供调用者使用。
type Job interface {
	// ID 返回Id的唯一标识符
	ID() uuid.UUID
	// LastRun 返回上次运行时间
	LastRun() (time.Time, error)
	// Name 返回在作业上定义的名称。
	Name() string
	// NextRun 返回下一次调度运行时间
	NextRun() (time.Time, error)
	// NextRuns 返回接下来的 N 次运行时间，如果调度表中不存在第m个运行时间，则根据第m-1个运算时间动态生成
	NextRuns(int) ([]time.Time, error)
	// RunNow 现在运行一次作业。
	// 这不会改变现有的运行时间表，并尊重所有作业和调度器的限制。
	// 这意味着现在运行作业可能会导致作业的常规间隔被重新安排，
	// 因为RunNow运行的实例阻塞了您的运行限制。
	RunNow() error
	// Tags 返回工作的string类型的标签列表。
	Tags() []string
}

var _ Job = (*job)(nil)

// job 是实现的内部结构
// 公共接口。这是用来避免泄露调用者永远不需要持有和考虑的信息
type job struct {
	id            uuid.UUID
	name          string
	tags          []string
	jobOutRequest chan jobOutRequest
	runJobRequest chan runJobRequest
}

func (j job) ID() uuid.UUID {
	return j.id
}

func (j job) LastRun() (time.Time, error) {
	ij := requestJob(j.id, j.jobOutRequest)
	if ij == nil || ij.id == uuid.Nil {
		return time.Time{}, ErrJobNotFound
	}
	return ij.lastRun, nil
}

func (j job) Name() string {
	return j.name
}

func (j job) NextRun() (time.Time, error) {
	ij := requestJob(j.id, j.jobOutRequest)
	if ij == nil || ij.id == uuid.Nil {
		return time.Time{}, ErrJobNotFound
	}
	if len(ij.nextScheduled) == 0 {
		return time.Time{}, nil
	}
	// the first element is the next scheduled run with subsequent
	// runs following after in the slice
	return ij.nextScheduled[0], nil
}

func (j job) NextRuns(count int) ([]time.Time, error) {
	ij := requestJob(j.id, j.jobOutRequest)
	if ij == nil || ij.id == uuid.Nil {
		return nil, ErrJobNotFound
	}

	lengthNextScheduled := len(ij.nextScheduled)
	if lengthNextScheduled == 0 {
		return nil, nil
	} else if count <= lengthNextScheduled {
		return ij.nextScheduled[:count], nil
	}

	out := make([]time.Time, count)
	for i := 0; i < count; i++ {
		if i < lengthNextScheduled {
			out[i] = ij.nextScheduled[i]
			continue
		}

		from := out[i-1]
		out[i] = ij.next(from)
	}

	return out, nil
}

func (j job) Tags() []string {
	return j.tags
}

func (j job) RunNow() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp := make(chan error, 1)

	select {
	case j.runJobRequest <- runJobRequest{
		id:      j.id,
		outChan: resp,
	}:
	case <-time.After(100 * time.Millisecond):
		return ErrJobRunNowFailed
	}
	var err error
	select {
	case <-ctx.Done():
		return ErrJobRunNowFailed
	case errReceived := <-resp:
		err = errReceived
	}
	return err
}
