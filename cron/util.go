package cron

import (
	"context"
	"reflect"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"
)

// callJobFuncWithParams 调用带参数的任务函数
func callJobFuncWithParams(jobFunc any, params ...any) error {
	if jobFunc == nil {
		return nil
	}
	f := reflect.ValueOf(jobFunc)
	if f.IsZero() {
		return nil
	}
	if len(params) != f.Type().NumIn() {
		return nil
	}
	in := make([]reflect.Value, len(params))
	for k, param := range params {
		in[k] = reflect.ValueOf(param)
	}
	returnValues := f.Call(in)
	for _, val := range returnValues {
		i := val.Interface()
		if err, ok := i.(error); ok {
			return err
		}
	}
	return nil
}

// requestJob 请求一个job
func requestJob(id uuid.UUID, ch chan jobOutRequest) *internalJob {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return requestJobCtx(ctx, id, ch)
}

// 函数返回一个指向 internalJob 的指针，如果成功获取到任务，它会返回该任务，否则（例如任务取消或超时）返回 nil
func requestJobCtx(ctx context.Context, id uuid.UUID, ch chan jobOutRequest) *internalJob {
	resp := make(chan internalJob, 1)
	select {
	case ch <- jobOutRequest{
		id:      id,
		outChan: resp,
	}:
	case <-ctx.Done():
		return nil
	}
	var j internalJob
	select {
	case <-ctx.Done():
		return nil
	case jobReceived := <-resp:
		j = jobReceived
	}
	return &j
}

// removeSliceDuplicatesInt 移除[]int中的重复元素
func removeSliceDuplicatesInt(in []int) []int {
	m := make(map[int]struct{})

	for _, i := range in {
		m[i] = struct{}{}
	}
	return maps.Keys(m)
}

// convertAtTimesToDateTime 将AtTimes转换为时间切片并按照升序排序
func convertAtTimesToDateTime(atTimes AtTimes, location *time.Location) ([]time.Time, error) {
	if atTimes == nil {
		return nil, errAtTimesNil
	}
	var atTimesDate []time.Time
	for _, a := range atTimes() {
		if a == nil {
			return nil, errAtTimeNil
		}
		at := a()
		if at.hours > 23 {
			return nil, errAtTimeHours
		} else if at.minutes > 59 || at.seconds > 59 {
			return nil, errAtTimeMinSec
		}
		atTimesDate = append(atTimesDate, at.time(location))
	}
	slices.SortStableFunc(atTimesDate, ascendingTime)
	return atTimesDate, nil
}

func ascendingTime(a, b time.Time) int {
	return a.Compare(b)
}

type waitGroupWithMutex struct {
	wg sync.WaitGroup
	mu sync.Mutex
}

func (w *waitGroupWithMutex) Add(delta int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.wg.Add(delta)
}

func (w *waitGroupWithMutex) Done() {
	w.wg.Done()
}

func (w *waitGroupWithMutex) Wait() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.wg.Wait()
}
