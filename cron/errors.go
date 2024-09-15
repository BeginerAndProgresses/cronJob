package cron

import "fmt"

// 公共错误定义
var (
	ErrCronJobInvalid                = fmt.Errorf("gocron： CronJob： 无效的 crontab")
	ErrCronJobParse                  = fmt.Errorf("gocron： CronJob： crontab 解析失败")
	ErrDailyJobAtTimeNil             = fmt.Errorf("gocron： DailyJob： atTime 在 atTimes 内不得为 nil")
	ErrDailyJobAtTimesNil            = fmt.Errorf("gocron： DailyJob： atTimes 不得为 nil")
	ErrDailyJobHours                 = fmt.Errorf("gocron： DailyJob： atTimes 小时必须介于 0 和 23 之间（包括 0 和 23 之间）")
	ErrDailyJobMinutesSeconds        = fmt.Errorf("gocron： DailyJob： atTimes 分钟和秒必须介于 0 和 59 之间（包括 0 和 59） 之间")
	ErrDurationJobIntervalZero       = fmt.Errorf("gocron： DurationJob： 时间间隔为 0")
	ErrDurationRandomJobMinMax       = fmt.Errorf("gocron： DurationRandomJob： 最小持续时间必须小于最大持续时间")
	ErrEventListenerFuncNil          = fmt.Errorf("gocron：eventListenerFunc 不能为 nil")
	ErrJobNotFound                   = fmt.Errorf("gocron：未找到作业")
	ErrJobRunNowFailed               = fmt.Errorf("gocron： Job： RunNow： 调度程序无法访问")
	ErrMonthlyJobDays                = fmt.Errorf("gocron： MonthlyJob： daysOfTheMonth 必须介于 31 和 -31 之间（含 31 和 -31），而不是 0")
	ErrMonthlyJobAtTimeNil           = fmt.Errorf("gocron： MonthlyJob： atTime 在 atTimes 内不得为 nil")
	ErrMonthlyJobAtTimesNil          = fmt.Errorf("gocron： MonthlyJob： atTimes 不得为 nil")
	ErrMonthlyJobDaysNil             = fmt.Errorf("gocron： MonthlyJob： daysOfTheMonth 不得为 nil")
	ErrMonthlyJobHours               = fmt.Errorf("gocron： MonthlyJob： atTimes 小时数必须介于 0 和 23 之间（包括 0 和 23 之间）")
	ErrMonthlyJobMinutesSeconds      = fmt.Errorf("gocron： MonthlyJob： atTimes 分钟和秒必须介于 0 和 59 之间（包括 0 和 59 之间）")
	ErrNewJobTaskNil                 = fmt.Errorf("gocron： NewJob： 任务不得为 nil")
	ErrNewJobTaskNotFunc             = fmt.Errorf("gocron： NewJob： Task.Function 必须是 reflect 类型。功能")
	ErrNewJobWrongNumberOfParameters = fmt.Errorf("gocron： NewJob： 提供的参数数量与预期不匹配")
	ErrNewJobWrongTypeOfParameters   = fmt.Errorf("gocron： NewJob： 提供的参数类型与预期不匹配")
	ErrOneTimeJobStartDateTimePast   = fmt.Errorf("gocron： OneTimeJob： start 不能是过去的时间")
	ErrStopExecutorTimedOut          = fmt.Errorf("gocron：等待 Executor 停止超时")
	ErrStopJobsTimedOut              = fmt.Errorf("gocron：等待作业完成超时")
	ErrStopSchedulerTimedOut         = fmt.Errorf("gocron：等待调度程序到 STO 超时p")
	ErrWeeklyJobAtTimeNil            = fmt.Errorf("gocron： WeeklyJob： atTime 内的 atTimes 不得为 nil")
	ErrWeeklyJobAtTimesNil           = fmt.Errorf("gocron： WeeklyJob： atTimes 不得为 nil")
	ErrWeeklyJobDaysOfTheWeekNil     = fmt.Errorf("gocron： WeeklyJob： daysOfTheWeek 不得为 nil")
	ErrWeeklyJobHours                = fmt.Errorf("gocron： WeeklyJob： atTimes 小时数必须介于 0 和 23 之间（包括 0 和 23 之间）")
	ErrWeeklyJobMinutesSeconds       = fmt.Errorf("gocron： WeeklyJob： atTimes 分钟和秒必须介于 0 和 59 之间（包括 0 和 59 之间）")
	ErrPanicRecovered                = fmt.Errorf("gocron：panic已恢复")
	ErrWithClockNil                  = fmt.Errorf("gocron： WithClock： clock 不能为 nil")
	ErrWithDistributedElectorNil     = fmt.Errorf("gocron： WithDistributedElector： elector 不能为 nil")
	ErrWithDistributedLockerNil      = fmt.Errorf("gocron： WithDistributedLocker： locker 不能为 nil")
	ErrWithDistributedJobLockerNil   = fmt.Errorf("gocron： WithDistributedJobLocker： locker 不能为 nil")
	ErrWithIdentifierNil             = fmt.Errorf("gocron： WithIdentifier： 标识符不得为 nil")
	ErrWithLimitConcurrentJobsZero   = fmt.Errorf("gocron： WithLimitConcurrentJobs： limit 必须大于 0")
	ErrWithLocationNil               = fmt.Errorf("gocron： WithLocation： location 不能为 nil")
	ErrWithLoggerNil                 = fmt.Errorf("gocron： WithLogger： logger 不能为 nil")
	ErrWithMonitorNil                = fmt.Errorf("gocron： WithMonitor： monitor 不能为 nil")
	ErrWithNameEmpty                 = fmt.Errorf("gocron： WithName： name 不能为空")
	ErrWithStartDateTimePast         = fmt.Errorf("gocron： WithStartDateTime： start 不能是过去的时间")
	ErrWithStopDateTimePast          = fmt.Errorf("gocron： WithStopDateTime： end 不能是过去的时间")
	ErrStartTimeLaterThanEndTime     = fmt.Errorf("gocron： WithStartDateTime： start 不得晚于 end")
	ErrStopTimeEarlierThanStartTime  = fmt.Errorf("gocron： WithStopDateTime： end 不得早于 start")
	ErrWithStopTimeoutZeroOrNegative = fmt.Errorf("gocron： WithStopTimeout： 超时必须大于 0")
)

// 内部错误
var (
	errAtTimeNil    = fmt.Errorf("errAtTimeNil")
	errAtTimesNil   = fmt.Errorf("errAtTimesNil")
	errAtTimeHours  = fmt.Errorf("errAtTimeHours")
	errAtTimeMinSec = fmt.Errorf("errAtTimeMinSec")
)
