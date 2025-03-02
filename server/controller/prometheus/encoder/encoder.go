/**
 * Copyright (c) 2023 Yunshan Networks
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package encoder

import (
	"context"
	"sync"
	"time"

	"github.com/op/go-logging"
	"golang.org/x/sync/errgroup"

	"github.com/deepflowio/deepflow/message/controller"
	. "github.com/deepflowio/deepflow/server/controller/prometheus/common"
	prometheuscfg "github.com/deepflowio/deepflow/server/controller/prometheus/config"
)

var log = logging.MustGetLogger("prometheus.synchronizer.encoder")

var (
	encoderOnce sync.Once
	encoder     *Encoder
)

type Encoder struct {
	ctx    context.Context
	cancel context.CancelFunc

	mux             sync.Mutex
	working         bool
	refreshInterval time.Duration

	metricName   *metricName
	labelName    *labelName
	labelValue   *labelValue
	labelLayout  *labelLayout
	label        *label
	metricLabel  *metricLabel
	metricTarget *metricTarget
	target       *target
}

func GetSingleton() *Encoder {
	encoderOnce.Do(func() {
		encoder = &Encoder{}
	})
	return encoder
}

func (e *Encoder) Init(ctx context.Context, cfg *prometheuscfg.Config) {
	log.Infof("init prometheus encoder")
	mCtx, mCancel := context.WithCancel(ctx)
	e.ctx = mCtx
	e.cancel = mCancel
	e.metricName = newMetricName(cfg.ResourceMaxID1)
	e.labelName = newLabelName(cfg.ResourceMaxID0)
	e.labelValue = newLabelValue(cfg.ResourceMaxID1)
	e.label = newLabel()
	e.labelLayout = newLabelLayout()
	e.metricLabel = newMetricLabel(e.label)
	e.target = newTarget(cfg.ResourceMaxID1)
	e.metricTarget = newMetricTarget(e.target)
	e.refreshInterval = time.Duration(cfg.EncoderCacheRefreshInterval) * time.Second
	return
}

func (e *Encoder) Start() error {
	e.mux.Lock()
	if e.working {
		return nil
	}
	e.working = true
	e.mux.Unlock()

	log.Info("prometheus encoder started")
	e.refresh()
	go func() {
		ticker := time.NewTicker(e.refreshInterval)
		for {
			select {
			case <-e.ctx.Done():
				return
			case <-ticker.C:
				e.refresh()
			}
		}
	}()
	return nil
}

func (e *Encoder) Stop() {
	if e.cancel != nil {
		e.cancel()
	}
	e.mux.Lock()
	e.working = false
	e.mux.Unlock()
	log.Info("prometheus encoder stopped")
}

func (e *Encoder) refresh() error {
	e.label.refresh()
	eg := &errgroup.Group{}
	AppendErrGroup(eg, e.metricName.refresh)
	AppendErrGroup(eg, e.labelName.refresh)
	AppendErrGroup(eg, e.labelValue.refresh)
	AppendErrGroup(eg, e.labelLayout.refresh)
	AppendErrGroup(eg, e.metricLabel.refresh)
	AppendErrGroup(eg, e.metricTarget.refresh)
	AppendErrGroup(eg, e.target.refresh)
	return eg.Wait()
}

func (e *Encoder) Encode(req *controller.SyncPrometheusRequest) (*controller.SyncPrometheusResponse, error) {
	resp := new(controller.SyncPrometheusResponse)
	egRunAhead := &errgroup.Group{}
	AppendErrGroup(egRunAhead, e.encodeLabel, resp, req.GetLabels())
	AppendErrGroup(egRunAhead, e.encodeLabelIndex, resp, req.GetMetricAppLabelLayouts())
	AppendErrGroup(egRunAhead, e.encodeTarget, resp, req.GetTargets())
	err := egRunAhead.Wait()
	if err != nil {
		return resp, err
	}
	eg := &errgroup.Group{}
	AppendErrGroup(eg, e.encodeMetricName, resp, req.GetMetricNames())
	AppendErrGroup(eg, e.encodeLabelName, resp, req.GetLabelNames())
	AppendErrGroup(eg, e.encodeLabelValue, resp, req.GetLabelValues())
	AppendErrGroup(eg, e.encodeMetricLabel, req.GetMetricLabels())
	AppendErrGroup(eg, e.encodeMetricTarget, resp, req.GetMetricTargets())
	err = eg.Wait()
	return resp, err
}

func (e *Encoder) encodeMetricName(args ...interface{}) error {
	resp := args[0].(*controller.SyncPrometheusResponse)
	names := args[1].([]string)
	mns, err := e.metricName.encode(names)
	if err != nil {
		return err
	}
	resp.MetricNames = mns
	return nil
}

func (e *Encoder) encodeLabelName(args ...interface{}) error {
	resp := args[0].(*controller.SyncPrometheusResponse)
	names := args[1].([]string)
	lns, err := e.labelName.encode(names)
	if err != nil {
		return err
	}
	resp.LabelNames = lns
	return nil
}

func (e *Encoder) encodeLabelValue(args ...interface{}) error {
	resp := args[0].(*controller.SyncPrometheusResponse)
	values := args[1].([]string)
	lvs, err := e.labelValue.encode(values)
	if err != nil {
		return err
	}
	resp.LabelValues = lvs
	return nil
}

func (e *Encoder) encodeLabelIndex(args ...interface{}) error {
	resp := args[0].(*controller.SyncPrometheusResponse)
	layouts := args[1].([]*controller.PrometheusMetricAPPLabelLayoutRequest)
	lis, err := e.labelLayout.encode(layouts)
	if err != nil {
		return err
	}
	resp.MetricAppLabelLayouts = lis
	return nil
}

func (e *Encoder) encodeLabel(args ...interface{}) error {
	resp := args[0].(*controller.SyncPrometheusResponse)
	labels := args[1].([]*controller.PrometheusLabelRequest)
	ls, err := e.label.encode(labels)
	if err != nil {
		return err
	}
	resp.Labels = ls
	return nil
}

func (e *Encoder) encodeMetricLabel(args ...interface{}) error {
	mls := args[0].([]*controller.PrometheusMetricLabelRequest)
	err := e.metricLabel.encode(mls)
	if err != nil {
		return err
	}
	return nil
}

func (e *Encoder) encodeMetricTarget(args ...interface{}) error {
	resp := args[0].(*controller.SyncPrometheusResponse)
	metricTargets := args[1].([]*controller.PrometheusMetricTargetRequest)
	mts, err := e.metricTarget.encode(metricTargets)
	if err != nil {
		return err
	}
	resp.MetricTargets = mts
	return nil
}

func (e *Encoder) encodeTarget(args ...interface{}) error {
	resp := args[0].(*controller.SyncPrometheusResponse)
	targets := args[1].([]*controller.PrometheusTargetRequest)
	ts, err := e.target.encode(targets)
	if err != nil {
		return err
	}
	resp.Targets = ts
	return nil
}
