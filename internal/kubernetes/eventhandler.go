/*
Copyright 2018 Planet Labs Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing permissions
and limitations under the License.
*/

package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
)

const (
	// DefaultDrainBuffer is the default minimum time between node drains.
	DefaultDrainBuffer = 10 * time.Minute

	eventReasonCordonStarting  = "CordonStarting"
	eventReasonCordonSucceeded = "CordonSucceeded"
	eventReasonCordonFailed    = "CordonFailed"

	eventReasonUncordonStarting  = "UncordonStarting"
	eventReasonUncordonSucceeded = "UncordonSucceeded"
	eventReasonUncordonFailed    = "UncordonFailed"

	eventReasonDrainScheduled        = "DrainScheduled"
	eventReasonDrainSchedulingFailed = "DrainSchedulingFailed"
	eventReasonDrainStarting         = "DrainStarting"
	eventReasonDrainSucceeded        = "DrainSucceeded"
	eventReasonDrainFailed           = "DrainFailed"

	tagResultSucceeded = "succeeded"
	tagResultFailed    = "failed"

	drainRetryAnnotationKey   = "draino/drain-retry"
	drainRetryAnnotationValue = "true"

	drainoConditionsAnnotationKey = "draino.planet.com/conditions"

	attrNodeName = "node_name"
	attrResult   = "result"
)

// Metrics holds OTel counters for draino operations.
type Metrics struct {
	Cordoned       metric.Int64Counter
	Uncordoned     metric.Int64Counter
	Drained        metric.Int64Counter
	DrainScheduled metric.Int64Counter
}

// InitMetrics creates the metric counters from a meter.
func InitMetrics(meter metric.Meter) (*Metrics, error) {
	cordoned, err := meter.Int64Counter(
		"draino_cordoned_nodes_total",
		metric.WithDescription("Number of nodes cordoned."),
		metric.WithUnit("{1}"),
	)
	if err != nil {
		return nil, err
	}
	uncordoned, err := meter.Int64Counter(
		"draino_uncordoned_nodes_total",
		metric.WithDescription("Number of nodes uncordoned."),
		metric.WithUnit("{1}"),
	)
	if err != nil {
		return nil, err
	}
	drained, err := meter.Int64Counter(
		"draino_drained_nodes_total",
		metric.WithDescription("Number of nodes drained."),
		metric.WithUnit("{1}"),
	)
	if err != nil {
		return nil, err
	}
	drainScheduled, err := meter.Int64Counter(
		"draino_drain_scheduled_nodes_total",
		metric.WithDescription("Number of nodes scheduled for drain."),
		metric.WithUnit("{1}"),
	)
	if err != nil {
		return nil, err
	}
	return &Metrics{
		Cordoned:       cordoned,
		Uncordoned:     uncordoned,
		Drained:        drained,
		DrainScheduled: drainScheduled,
	}, nil
}

// WithMetrics configures a DrainingResourceEventHandler with OTel metrics.
func WithMetrics(m *Metrics) DrainingResourceEventHandlerOption {
	return func(h *DrainingResourceEventHandler) {
		h.metrics = m
	}
}

func recordMetric(ctx context.Context, m *Metrics, counterFn func(*Metrics) metric.Int64Counter, nodeName, result string) {
	if m == nil {
		return
	}
	counter := counterFn(m)
	if counter == nil {
		return
	}
	counter.Add(ctx, 1, metric.WithAttributes(
		attribute.String(attrNodeName, nodeName),
		attribute.String(attrResult, result),
	))
}

// A DrainingResourceEventHandler cordons and drains any added or updated nodes.
type DrainingResourceEventHandler struct {
	ctx            context.Context
	logger         *zap.Logger
	cordonDrainer  CordonDrainer
	eventRecorder  record.EventRecorder
	drainScheduler DrainScheduler

	buffer                time.Duration

	conditions []SuppliedCondition
	metrics    *Metrics
}

// DrainingResourceEventHandlerOption configures an DrainingResourceEventHandler.
type DrainingResourceEventHandlerOption func(d *DrainingResourceEventHandler)

// WithLogger configures a DrainingResourceEventHandler to use the supplied
// logger.
func WithLogger(l *zap.Logger) DrainingResourceEventHandlerOption {
	return func(h *DrainingResourceEventHandler) {
		h.logger = l
	}
}

// WithDrainBuffer configures the minimum time between scheduled drains.
func WithDrainBuffer(d time.Duration) DrainingResourceEventHandlerOption {
	return func(h *DrainingResourceEventHandler) {
		h.buffer = d
	}
}

// MustParseConditions calls ParseConditions and panics on error.
func MustParseConditions(conditions []string) []SuppliedCondition {
	parsed, err := ParseConditions(conditions)
	if err != nil {
		panic(err)
	}
	return parsed
}

// WithConditionsFilter configures which conditions should be handled.
func WithConditionsFilter(conditions []string) DrainingResourceEventHandlerOption {
	return func(h *DrainingResourceEventHandler) {
		h.conditions = MustParseConditions(conditions)
	}
}

// NewDrainingResourceEventHandler returns a new DrainingResourceEventHandler.
func NewDrainingResourceEventHandler(d CordonDrainer, e record.EventRecorder, ho ...DrainingResourceEventHandlerOption) *DrainingResourceEventHandler {
	h := &DrainingResourceEventHandler{
		ctx:                   context.Background(),
		logger:                zap.NewNop(),
		cordonDrainer:         d,
		eventRecorder:         e,
		buffer:                DefaultDrainBuffer,
	}
	for _, o := range ho {
		o(h)
	}
	h.drainScheduler = NewDrainSchedules(d, e, h.buffer, h.logger, h.metrics)
	return h
}

// OnAdd cordons and drains the added node.
func (h *DrainingResourceEventHandler) OnAdd(obj interface{}, isInInitialList bool) {
	n, ok := obj.(*core.Node)
	if !ok {
		return
	}
	h.HandleNode(n)
}

// OnUpdate cordons and drains the updated node.
func (h *DrainingResourceEventHandler) OnUpdate(oldObj, newObj interface{}) {
	old, okOld := oldObj.(*core.Node)
	new, okNew := newObj.(*core.Node)
	if !okNew {
		return
	}
	if okOld && old.GetResourceVersion() == new.GetResourceVersion() {
		return
	}
	h.OnAdd(newObj, false)
}

// OnDelete removes any pending drain schedule for the deleted node.

func (h *DrainingResourceEventHandler) OnDelete(obj interface{}) {
	n, ok := obj.(*core.Node)
	if !ok {
		d, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		h.drainScheduler.DeleteSchedule(d.Key)
		return
	}

	h.drainScheduler.DeleteSchedule(n.GetName())
}

func (h *DrainingResourceEventHandler) HandleNode(n *core.Node) {
	badConditions := h.offendingConditions(n)
	if len(badConditions) == 0 {
		if h.shouldUncordon(n) {
			h.drainScheduler.DeleteSchedule(n.GetName())
			h.uncordon(n)
		}
		return
	}

	// First cordon the node if it is not yet cordonned
	if !n.Spec.Unschedulable {
		h.cordon(n, badConditions)
	}

	// Let's ensure that a drain is scheduled
	hasSchedule, failedDrain := h.drainScheduler.HasSchedule(n.GetName())
	if !hasSchedule {
		h.scheduleDrain(n)
		return
	}

	// Is there a request to retry a failed drain activity. If yes reschedule drain
	if failedDrain && HasDrainRetryAnnotation(n) {
		h.drainScheduler.DeleteSchedule(n.GetName())
		h.scheduleDrain(n)
		return
	}
}

func (h *DrainingResourceEventHandler) offendingConditions(n *core.Node) []SuppliedCondition {
	var conditions []SuppliedCondition
	for _, suppliedCondition := range h.conditions {
		for _, nodeCondition := range n.Status.Conditions {
			if suppliedCondition.Type == nodeCondition.Type &&
				suppliedCondition.Status == nodeCondition.Status &&
				time.Since(nodeCondition.LastTransitionTime.Time) >= suppliedCondition.MinimumDuration {
				conditions = append(conditions, suppliedCondition)
			}
		}
	}
	return conditions
}

func (h *DrainingResourceEventHandler) shouldUncordon(n *core.Node) bool {
	if !n.Spec.Unschedulable {
		return false
	}
	previousConditions := parseConditionsFromAnnotation(n, h.logger)
	if len(previousConditions) == 0 {
		return false
	}
	for _, previousCondition := range previousConditions {
		for _, nodeCondition := range n.Status.Conditions {
			if previousCondition.Type == nodeCondition.Type &&
				previousCondition.Status != nodeCondition.Status &&
				time.Since(nodeCondition.LastTransitionTime.Time) >= previousCondition.MinimumDuration {
				return true
			}
		}
	}
	return false
}

func parseConditionsFromAnnotation(n *core.Node, log *zap.Logger) []SuppliedCondition {
	if n.Annotations == nil {
		return nil
	}
	if n.Annotations[drainoConditionsAnnotationKey] == "" {
		return nil
	}
	rawConditions := strings.Split(n.Annotations[drainoConditionsAnnotationKey], ";")
	parsed, err := ParseConditions(rawConditions)
	if err != nil {
		log.Warn("Failed to parse conditions from annotation, skipping uncordon check",
			zap.String("annotation", drainoConditionsAnnotationKey),
			zap.String("value", n.Annotations[drainoConditionsAnnotationKey]),
			zap.Error(err))
		return nil
	}
	return parsed
}

func (h *DrainingResourceEventHandler) uncordon(n *core.Node) {
	log := h.logger.With(zap.String("node", n.GetName()))
	nr := &core.ObjectReference{Kind: "Node", Name: n.GetName(), UID: types.UID(n.GetName())}

	log.Debug("Uncordoning")
	h.eventRecorder.Event(nr, core.EventTypeWarning, eventReasonUncordonStarting, "Uncordoning node")
	if err := h.cordonDrainer.Uncordon(n, removeAnnotationMutator); err != nil {
		log.Info("Failed to uncordon", zap.Error(err))
		recordMetric(h.ctx, h.metrics, func(m *Metrics) metric.Int64Counter { return m.Uncordoned }, n.GetName(), tagResultFailed)
		h.eventRecorder.Eventf(nr, core.EventTypeWarning, eventReasonUncordonFailed, "Uncordoning failed: %v", err)
		return
	}
	log.Info("Uncordoned")
	recordMetric(h.ctx, h.metrics, func(m *Metrics) metric.Int64Counter { return m.Uncordoned }, n.GetName(), tagResultSucceeded)
	h.eventRecorder.Event(nr, core.EventTypeNormal, eventReasonUncordonSucceeded, "Uncordoned node")
}

func removeAnnotationMutator(n *core.Node) {
	delete(n.Annotations, drainoConditionsAnnotationKey)
}

func (h *DrainingResourceEventHandler) cordon(n *core.Node, badConditions []SuppliedCondition) {
	log := h.logger.With(zap.String("node", n.GetName()))
	// Events must be associated with this object reference, rather than the
	// node itself, in order to appear under `kubectl describe node` due to the
	// way that command is implemented.
	// https://github.com/kubernetes/kubernetes/blob/17740a2/pkg/printers/internalversion/describe.go#L2711
	nr := &core.ObjectReference{Kind: "Node", Name: n.GetName(), UID: types.UID(n.GetName())}

	log.Debug("Cordoning")
	h.eventRecorder.Event(nr, core.EventTypeWarning, eventReasonCordonStarting, "Cordoning node")
	if err := h.cordonDrainer.Cordon(n, conditionAnnotationMutator(badConditions)); err != nil {
		log.Info("Failed to cordon", zap.Error(err))
		recordMetric(h.ctx, h.metrics, func(m *Metrics) metric.Int64Counter { return m.Cordoned }, n.GetName(), tagResultFailed)
		h.eventRecorder.Eventf(nr, core.EventTypeWarning, eventReasonCordonFailed, "Cordoning failed: %v", err)
		return
	}
	log.Info("Cordoned")
	recordMetric(h.ctx, h.metrics, func(m *Metrics) metric.Int64Counter { return m.Cordoned }, n.GetName(), tagResultSucceeded)
	h.eventRecorder.Event(nr, core.EventTypeNormal, eventReasonCordonSucceeded, "Cordoned node")
}

func conditionAnnotationMutator(conditions []SuppliedCondition) func(*core.Node) {
	var value []string
	for _, c := range conditions {
		value = append(value, fmt.Sprintf("%v=%v,%v", c.Type, c.Status, c.MinimumDuration))
	}
	return func(n *core.Node) {
		if n.Annotations == nil {
			n.Annotations = make(map[string]string)
		}
		n.Annotations[drainoConditionsAnnotationKey] = strings.Join(value, ";")
	}
}

// drain schedule the draining activity
func (h *DrainingResourceEventHandler) scheduleDrain(n *core.Node) {
	log := h.logger.With(zap.String("node", n.GetName()))
	nr := &core.ObjectReference{Kind: "Node", Name: n.GetName(), UID: types.UID(n.GetName())}
	log.Debug("Scheduling drain")
	when, err := h.drainScheduler.Schedule(n)
	if err != nil {
		if IsAlreadyScheduledError(err) {
			return
		}
		log.Info("Failed to schedule the drain activity", zap.Error(err))
		recordMetric(h.ctx, h.metrics, func(m *Metrics) metric.Int64Counter { return m.DrainScheduled }, n.GetName(), tagResultFailed)
		h.eventRecorder.Eventf(nr, core.EventTypeWarning, eventReasonDrainSchedulingFailed, "Drain scheduling failed: %v", err)
		return
	}
	log.Info("Drain scheduled ", zap.Time("after", when))
	recordMetric(h.ctx, h.metrics, func(m *Metrics) metric.Int64Counter { return m.DrainScheduled }, n.GetName(), tagResultSucceeded)
	h.eventRecorder.Eventf(nr, core.EventTypeWarning, eventReasonDrainScheduled, "Will drain node after %s", when.Format(time.RFC3339Nano))
}

func HasDrainRetryAnnotation(n *core.Node) bool {
	annotations := n.GetAnnotations()
	if annotations == nil {
		return false
	}
	return annotations[drainRetryAnnotationKey] == drainRetryAnnotationValue
}
