package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
)

const (
	SetConditionTimeout     = 10 * time.Second
	SetConditionRetryPeriod = 50 * time.Millisecond
)

type DrainScheduler interface {
	HasSchedule(name string) (has, failed bool)
	Schedule(node *v1.Node) (time.Time, error)
	DeleteSchedule(name string)
}

type DrainSchedules struct {
	sync.Mutex
	schedules map[string]*schedule

	lastDrainScheduledFor time.Time
	period                time.Duration

	logger        *zap.Logger
	drainer       Drainer
	eventRecorder record.EventRecorder
	metrics       *Metrics
}

func NewDrainSchedules(drainer Drainer, eventRecorder record.EventRecorder, period time.Duration, logger *zap.Logger, metrics *Metrics) DrainScheduler {
	return &DrainSchedules{
		schedules:     map[string]*schedule{},
		period:        period,
		logger:        logger,
		drainer:       drainer,
		eventRecorder: eventRecorder,
		metrics:       metrics,
	}
}

func (d *DrainSchedules) HasSchedule(name string) (has, failed bool) {
	d.Lock()
	defer d.Unlock()
	sched, ok := d.schedules[name]
	if !ok {
		return false, false
	}
	return true, sched.isFailed()
}

func (d *DrainSchedules) DeleteSchedule(name string) {
	d.Lock()
	defer d.Unlock()
	if s, ok := d.schedules[name]; ok {
		s.timer.Stop()
	} else {
		d.logger.Error("Failed schedule deletion", zap.String("key", name))
	}
	delete(d.schedules, name)
}

func (d *DrainSchedules) WhenNextSchedule() time.Time {
	// compute drain schedule time
	sooner := time.Now().Add(SetConditionTimeout + time.Second)
	when := d.lastDrainScheduledFor.Add(d.period)
	if when.Before(sooner) {
		when = sooner
	}
	return when
}

func (d *DrainSchedules) Schedule(node *v1.Node) (time.Time, error) {
	d.Lock()
	if sched, ok := d.schedules[node.GetName()]; ok {
		d.Unlock()
		return sched.when, NewAlreadyScheduledError() // we already have a schedule planned
	}

	// compute drain schedule time
	when := d.WhenNextSchedule()
	d.lastDrainScheduledFor = when
	d.schedules[node.GetName()] = d.newSchedule(node, when)
	d.Unlock()

	// Mark the node with the condition stating that drain is scheduled
	if err := RetryWithTimeout(
		func() error {
			return d.drainer.MarkDrain(node, when, time.Time{}, false)
		},
		SetConditionRetryPeriod,
		SetConditionTimeout,
	); err != nil {
		// if we cannot mark the node, let's remove the schedule
		d.DeleteSchedule(node.GetName())
		return time.Time{}, err
	}
	return when, nil
}

type schedule struct {
	when   time.Time
	failed int32
	finish time.Time
	timer  *time.Timer
}

func (s *schedule) setFailed() {
	atomic.StoreInt32(&s.failed, 1)
}

func (s *schedule) isFailed() bool {
	return atomic.LoadInt32(&s.failed) == 1
}

func (d *DrainSchedules) newSchedule(node *v1.Node, when time.Time) *schedule {
	sched := &schedule{
		when: when,
	}
	sched.timer = time.AfterFunc(time.Until(when), func() {
		log := d.logger.With(zap.String("node", node.GetName()))
		nr := &v1.ObjectReference{Kind: "Node", Name: node.GetName(), UID: types.UID(node.GetName())}
		d.eventRecorder.Event(nr, v1.EventTypeWarning, eventReasonDrainStarting, "Draining node")
		if err := d.drainer.Drain(node); err != nil {
			sched.finish = time.Now()
			sched.setFailed()
			log.Info("Failed to drain", zap.Error(err))
			recordMetric(context.Background(), d.metrics, func(m *Metrics) metric.Int64Counter { return m.Drained }, node.GetName(), tagResultFailed)
			d.eventRecorder.Eventf(nr, v1.EventTypeWarning, eventReasonDrainFailed, "Draining failed: %v", err)
			if markErr := RetryWithTimeout(
				func() error {
					return d.drainer.MarkDrain(node, when, sched.finish, true)
				},
				SetConditionRetryPeriod,
				SetConditionTimeout,
			); markErr != nil {
				log.Error("Failed to place condition following drain failure")
			}
			return
		}
		sched.finish = time.Now()
		log.Info("Drained")
		recordMetric(context.Background(), d.metrics, func(m *Metrics) metric.Int64Counter { return m.Drained }, node.GetName(), tagResultSucceeded)
		d.eventRecorder.Event(nr, v1.EventTypeNormal, eventReasonDrainSucceeded, "Drained node")
		if markErr := RetryWithTimeout(
			func() error {
				return d.drainer.MarkDrain(node, when, sched.finish, false)
			},
			SetConditionRetryPeriod,
			SetConditionTimeout,
		); markErr != nil {
			d.eventRecorder.Eventf(nr, v1.EventTypeWarning, eventReasonDrainFailed, "Failed to place drain condition: %v", markErr)
			log.Error("Failed to place condition following drain success", zap.Error(markErr))
		}
	})
	return sched
}

type AlreadyScheduledError struct {
	error
}

func (e *AlreadyScheduledError) Unwrap() error {
	return e.error
}

func NewAlreadyScheduledError() error {
	return &AlreadyScheduledError{
		fmt.Errorf("drain schedule is already planned for that node"),
	}
}
func IsAlreadyScheduledError(err error) bool {
	var target *AlreadyScheduledError
	return errors.As(err, &target)
}
