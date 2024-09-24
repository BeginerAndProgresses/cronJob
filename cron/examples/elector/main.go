package main

import (
	"context"
	"fmt"
	"github.com/BeginerAndProgresses/cronJob/cron"
	"log"
	"time"
)

var _ cron.Elector = (*myElector)(nil)

type myElector struct {
	num    int
	leader bool
}

func (m myElector) IsLeader(_ context.Context) error {
	if m.leader {
		log.Printf("node %d is leader", m.num)
		return nil
	}
	log.Printf("node %d is not leader", m.num)
	return fmt.Errorf("not leader")
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	for i := 0; i < 3; i++ {
		go func(i int) {
			elector := &myElector{
				num: i,
			}
			if i == 0 {
				elector.leader = true
			}

			scheduler, err := cron.NewScheduler(
				cron.WithDistributedElector(elector),
			)
			if err != nil {
				log.Println(err)
				return
			}

			_, err = scheduler.NewJob(
				cron.DurationJob(time.Second),
				cron.NewTask(func() {
					log.Println("run job")
				}),
			)
			if err != nil {
				log.Println(err)
				return
			}
			scheduler.Start()

			if i == 0 {
				time.Sleep(5 * time.Second)
				elector.leader = false
			}
			if i == 1 {
				time.Sleep(5 * time.Second)
				elector.leader = true
			}
		}(

			i)
	}

	select {} // wait forever
}
