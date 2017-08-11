package checker

import (
	"fmt"
	"github.com/moira-alert/moira-alert"
	"math"
	"time"
)

var checkPointGap int64 = 120

type TriggerChecker struct {
	TriggerId string
	Database  moira.Database
	Logger    moira.Logger
	Config    *Config

	maintenance int64
	trigger     *moira.Trigger
	isSimple    bool
	ttl         *int64
	ttlState    string
	lastCheck   *moira.CheckData
}

func (triggerChecker *TriggerChecker) Check(from *int64, now *int64) error {
	if now == nil {
		n := time.Now().Unix()
		now = &n
	}

	initialized, err := triggerChecker.initTrigger(from, *now)
	if err != nil || !initialized {
		return err
	}

	var fromTime int64
	if from == nil {
		fromTime = *triggerChecker.lastCheck.Timestamp
	}

	if triggerChecker.ttl != nil {
		fromTime = fromTime - *triggerChecker.ttl
	} else {
		fromTime = fromTime - 600
	}

	checkData, err := triggerChecker.handleTrigger(fromTime, *now)
	if err != nil {
		triggerChecker.Logger.Errorf("Trigger check failed: %s", err.Error())
		checkData = &moira.CheckData{
			Metrics:   triggerChecker.lastCheck.Metrics,
			State:     EXCEPTION,
			Timestamp: now,
			Score:     triggerChecker.lastCheck.Score,
			Message:   "Trigger evaluation exception",
		}
		//todo compare_states
		return nil //todo is it right?
	}

	checkData.Score = scores[checkData.State]
	for _, metricData := range checkData.Metrics{
		checkData.Score += scores[metricData.State]
	}
	triggerChecker.Database.SetTriggerLastCheck(triggerChecker.TriggerId, checkData)
	return nil
}

func (triggerChecker *TriggerChecker) handleTrigger(from, until int64) (*moira.CheckData, error) {
	checkData := &moira.CheckData{
		Metrics:   triggerChecker.lastCheck.Metrics,
		State:     OK,
		Timestamp: &until,
		Score:     triggerChecker.lastCheck.Score,
	}

	triggerTimeSeries, metrics, err := triggerChecker.getTimeSeries(from, until)
	if err != nil {
		return checkData, err
	}

	triggerChecker.cleanupMetricsValues(metrics, until)

	if len(triggerTimeSeries) == 0 && triggerChecker.ttl != nil {
		checkData.State = triggerChecker.ttlState
		checkData.Message = "Trigger has no metrics"
		return checkData, nil
	}

	for _, timeSeries := range triggerTimeSeries[1] {
		triggerChecker.Logger.Debugf("Checking timeSeries %s: %v", timeSeries.Name, timeSeries.Values)
		triggerChecker.Logger.Debugf("Checking interval: %v - %v (%vs), step: %v", timeSeries.StartTime, timeSeries.StopTime, timeSeries.StepTime, timeSeries.StopTime-timeSeries.StartTime)
		metricState, ok := checkData.Metrics[timeSeries.Name]
		if !ok {
			triggerChecker.Logger.Debugf("No metric state for %s", timeSeries.Name)
			metricState = moira.MetricData{
				State:     NODATA,
				Timestamp: int64(timeSeries.StartTime - 3600),
			}
		}
		checkPoint := math.Max(float64(metricState.Timestamp-checkPointGap), float64(metricState.EventTimestamp))
		triggerChecker.Logger.Debugf("Checkpoint for %s: %v", timeSeries.Name, checkPoint)

		for valueTimestamp := timeSeries.StartTime; valueTimestamp < int32(until)+timeSeries.StepTime; valueTimestamp += timeSeries.StepTime {
			cp := int32(checkPoint)
			if valueTimestamp < cp {
				continue
			}
			expressionValues, noEmptyValues := triggerTimeSeries.getExpressionValues(timeSeries, cp)
			triggerChecker.Logger.Debugf("values for ts %s: %v", valueTimestamp, expressionValues)
			if noEmptyValues {
				continue
			}

			expressionValues["warn_value"] = getMathFloat64Pointer(triggerChecker.trigger.WarnValue)
			expressionValues["error_value"] = getMathFloat64Pointer(triggerChecker.trigger.ErrorValue)
			expressionValues["PREV_STATE"] = 1000 //todo NODATA

			expressionState := GetExpression(triggerChecker.trigger.Expression, expressionValues)
			triggerTimeSeries.updateCheckData(timeSeries, checkData, expressionState, expressionValues, valueTimestamp)
			//todo compare_states
		}

		lastCheckTimeStamp := triggerChecker.lastCheck.Timestamp
		ttl := triggerChecker.ttl

		//compare with last_check timestamp in case if we have not run checker for a long time
		if ttl != nil && metricState.Timestamp+*triggerChecker.ttl < moira.UseInt64(lastCheckTimeStamp) {
			triggerChecker.Logger.Infof("Metric %s TTL expired for state %v", timeSeries.Name, metricState)
			if triggerChecker.ttlState == DEL && metricState.EventTimestamp != 0 {
				triggerChecker.Logger.Infof("Remove metric %s", timeSeries.Name)
				delete(checkData.Metrics, timeSeries.Name)
				if err := triggerChecker.Database.RemovePatternsMetrics(triggerChecker.trigger.Patterns); err != nil {
					return checkData, err
				}
				continue
			}
			triggerTimeSeries.updateCheckData(timeSeries, checkData, toMetricState(triggerChecker.ttlState), nil, int32(*triggerChecker.lastCheck.Timestamp-*triggerChecker.ttl))
			//todo compareStates
		}
	}
	return checkData, nil
}

func (triggerChecker *TriggerChecker) cleanupMetricsValues(metrics []string, until int64) {
	for _, metric := range metrics {
		go func(metric string) {
			//todo cache cache_ttl
			if err := triggerChecker.Database.CleanupMetricValues(metric, until-triggerChecker.Config.MetricsTTL); err != nil {
				triggerChecker.Logger.Error(err.Error())
			}
		}(metric)
	}
}

func (triggerChecker *TriggerChecker) getTimeSeries(from, until int64) (triggerTimeSeries, []string, error) {
	targets := triggerChecker.trigger.Targets
	var targetTimeSeries triggerTimeSeries = make(map[int][]*TimeSeries)
	targetNumber := 1
	metricsArr := make([]string, 0)

	for _, target := range targets {
		metricDatas, metrics, err := EvaluateTarget(triggerChecker.Database, target, from, until, triggerChecker.isSimple)
		if err != nil {
			return nil, nil, err
		}

		if targetNumber > 1 {
			if len(metricDatas) == 0 {
				return nil, nil, fmt.Errorf("Target #%v has no timeseries", targetNumber)
			} else if len(metricDatas) > 1 {
				return nil, nil, fmt.Errorf("Target #%v has more than one timeseries", targetNumber)
			}
		}

		targetTimeSeries[targetNumber] = metricDatas
		metricsArr = append(metricsArr, metrics...)
		targetNumber += 1
	}
	return targetTimeSeries, metricsArr, nil
}

func (triggerChecker *TriggerChecker) initTrigger(fromTime *int64, now int64) (bool, error) {
	trigger, err := triggerChecker.Database.GetTrigger(triggerChecker.TriggerId)
	if err != nil {
		return false, err
	}
	if trigger == nil {
		return false, nil
	}

	triggerChecker.trigger = trigger
	triggerChecker.isSimple = trigger.IsSimpleTrigger

	tagDatas, err := triggerChecker.Database.GetTags(trigger.Tags)
	if err != nil {
		return false, err
	}

	for _, tagData := range tagDatas {
		if tagData.Maintenance != nil && *tagData.Maintenance > triggerChecker.maintenance {
			triggerChecker.maintenance = *tagData.Maintenance
			break
		}
	}

	triggerChecker.ttl = trigger.Ttl
	if trigger.TtlState != nil {
		triggerChecker.ttlState = *trigger.TtlState
	} else {
		triggerChecker.ttlState = NODATA
	}

	triggerChecker.lastCheck, err = triggerChecker.Database.GetTriggerLastCheck(triggerChecker.TriggerId)
	if err != nil {
		return false, err
	}

	var begin int64
	if fromTime != nil {
		begin = *fromTime - 3600
	} else {
		begin = now - 3600
	}
	if triggerChecker.lastCheck == nil {
		triggerChecker.lastCheck = &moira.CheckData{
			Metrics:   make(map[string]moira.MetricData),
			State:     NODATA,
			Timestamp: &begin,
		}
	}

	if triggerChecker.lastCheck.Timestamp == nil {
		triggerChecker.lastCheck.Timestamp = &begin
	}

	return true, nil
}

func getMathFloat64Pointer(val *float64) float64 {
	if val != nil {
		return *val
	} else {
		return math.NaN()
	}
}