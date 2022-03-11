// Copyright  The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prometheusremotewrite // import "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/translator/prometheusremotewrite"

import (
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/timestamp"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/prompb"
	"go.opentelemetry.io/collector/model/pdata"
	conventions "go.opentelemetry.io/collector/model/semconv/v1.6.1"
)

const (
	nameStr     = "__name__"
	sumStr      = "_sum"
	countStr    = "_count"
	bucketStr   = "_bucket"
	leStr       = "le"
	quantileStr = "quantile"
	pInfStr     = "+Inf"
	keyStr      = "key"
)

type bucketBoundsData struct {
	sig   string
	bound float64
}

// byBucketBoundsData enables the usage of sort.Sort() with a slice of bucket bounds
type byBucketBoundsData []bucketBoundsData

func (m byBucketBoundsData) Len() int           { return len(m) }
func (m byBucketBoundsData) Less(i, j int) bool { return m[i].bound < m[j].bound }
func (m byBucketBoundsData) Swap(i, j int)      { m[i], m[j] = m[j], m[i] }

// ByLabelName enables the usage of sort.Sort() with a slice of labels
type ByLabelName []prompb.Label

func (a ByLabelName) Len() int           { return len(a) }
func (a ByLabelName) Less(i, j int) bool { return a[i].Name < a[j].Name }
func (a ByLabelName) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

// addSample finds a TimeSeries in tsMap that corresponds to the label set labels, and add sample to the TimeSeries; it
// creates a new TimeSeries in the map if not found and returns the time series signature.
// tsMap will be unmodified if either labels or sample is nil, but can still be modified if the exemplar is nil.
func addSample(tsMap map[string]*prompb.TimeSeries, sample *prompb.Sample, labels []prompb.Label,
	metric pdata.Metric) string {

	if sample == nil || labels == nil || tsMap == nil {
		return ""
	}

	sig := timeSeriesSignature(metric, &labels)
	ts, ok := tsMap[sig]

	if ok {
		ts.Samples = append(ts.Samples, *sample)
	} else {
		newTs := &prompb.TimeSeries{
			Labels:  labels,
			Samples: []prompb.Sample{*sample},
		}
		tsMap[sig] = newTs
	}

	return sig
}

// addExemplars finds a bucket bound that corresponds to the exemplars value and add the exemplar to the specific sig;
// we only add exemplars if samples are presents
// tsMap is unmodified if either of its parameters is nil and samples are nil.
func addExemplars(tsMap map[string]*prompb.TimeSeries, exemplars []prompb.Exemplar, bucketBoundsData []bucketBoundsData) {
	if tsMap == nil || bucketBoundsData == nil || exemplars == nil {
		return
	}

	sort.Sort(byBucketBoundsData(bucketBoundsData))

	for _, exemplar := range exemplars {
		addExemplar(tsMap, bucketBoundsData, exemplar)
	}
}

func addExemplar(tsMap map[string]*prompb.TimeSeries, bucketBounds []bucketBoundsData, exemplar prompb.Exemplar) {
	for _, bucketBound := range bucketBounds {
		sig := bucketBound.sig
		bound := bucketBound.bound

		_, ok := tsMap[sig]
		if ok {
			if tsMap[sig].Samples != nil {
				if tsMap[sig].Exemplars == nil {
					tsMap[sig].Exemplars = make([]prompb.Exemplar, 0)
				}

				if exemplar.Value <= bound {
					tsMap[sig].Exemplars = append(tsMap[sig].Exemplars, exemplar)
					return
				}
			}
		}
	}
}

// timeSeries return a string signature in the form of:
// 		TYPE-label1-value1- ...  -labelN-valueN
// the label slice should not contain duplicate label names; this method sorts the slice by label name before creating
// the signature.
func timeSeriesSignature(metric pdata.Metric, labels *[]prompb.Label) string {
	b := strings.Builder{}
	b.WriteString(metric.DataType().String())

	sort.Sort(ByLabelName(*labels))

	for _, lb := range *labels {
		b.WriteString("-")
		b.WriteString(lb.GetName())
		b.WriteString("-")
		b.WriteString(lb.GetValue())
	}

	return b.String()
}

// createAttributes creates a slice of Cortex Label with OTLP attributes and pairs of string values.
// Unpaired string value is ignored. String pairs overwrites OTLP labels if collision happens, and the overwrite is
// logged. Resultant label names are sanitized.
func createAttributes(resource pdata.Resource, attributes pdata.AttributeMap, externalLabels map[string]string, extras ...string) []prompb.Label {
	// map ensures no duplicate label name
	l := map[string]prompb.Label{}

	// Map service.name + service.namespace to job
	if serviceName, ok := resource.Attributes().Get(conventions.AttributeServiceName); ok {
		val := serviceName.AsString()
		if serviceNamespace, ok := resource.Attributes().Get(conventions.AttributeServiceNamespace); ok {
			val = fmt.Sprintf("%s/%s", serviceNamespace.AsString(), val)
		}
		l[model.JobLabel] = prompb.Label{
			Name:  model.JobLabel,
			Value: val,
		}
	}
	// Map service.instance.id to instance
	if instance, ok := resource.Attributes().Get(conventions.AttributeServiceInstanceID); ok {
		l[model.InstanceLabel] = prompb.Label{
			Name:  model.InstanceLabel,
			Value: instance.AsString(),
		}
	}

	// Ensure attributes are sorted by key for consistent merging of keys which
	// collide when sanitized.
	attributes.Sort()
	attributes.Range(func(key string, value pdata.AttributeValue) bool {
		if existingLabel, alreadyExists := l[sanitize(key)]; alreadyExists {
			existingLabel.Value = existingLabel.Value + ";" + value.AsString()
			l[sanitize(key)] = existingLabel
		} else {
			l[sanitize(key)] = prompb.Label{
				Name:  sanitize(key),
				Value: value.AsString(),
			}
		}

		return true
	})

	for key, value := range externalLabels {
		// External labels have already been sanitized
		if _, alreadyExists := l[key]; alreadyExists {
			// Skip external labels if they are overridden by metric attributes
			continue
		}
		l[key] = prompb.Label{
			Name:  key,
			Value: value,
		}
	}

	for i := 0; i < len(extras); i += 2 {
		if i+1 >= len(extras) {
			break
		}
		_, found := l[extras[i]]
		if found {
			log.Println("label " + extras[i] + " is overwritten. Check if Prometheus reserved labels are used.")
		}
		// internal labels should be maintained
		name := extras[i]
		if !(len(name) > 4 && name[:2] == "__" && name[len(name)-2:] == "__") {
			name = sanitize(name)
		}
		l[name] = prompb.Label{
			Name:  name,
			Value: extras[i+1],
		}
	}

	s := make([]prompb.Label, 0, len(l))
	for _, lb := range l {
		s = append(s, lb)
	}

	return s
}

// getPromMetricName creates a Prometheus metric name by attaching namespace prefix for Monotonic metrics.
func getPromMetricName(metric pdata.Metric, ns string) string {
	name := metric.Name()
	if len(ns) > 0 {
		name = ns + "_" + name
	}

	return sanitize(name)
}

// validateMetrics returns a bool representing whether the metric has a valid type and temporality combination and a
// matching metric type and field
func validateMetrics(metric pdata.Metric) bool {
	switch metric.DataType() {
	case pdata.MetricDataTypeGauge:
		return metric.Gauge().DataPoints().Len() != 0
	case pdata.MetricDataTypeSum:
		return metric.Sum().DataPoints().Len() != 0 && metric.Sum().AggregationTemporality() == pdata.MetricAggregationTemporalityCumulative
	case pdata.MetricDataTypeHistogram:
		return metric.Histogram().DataPoints().Len() != 0 && metric.Histogram().AggregationTemporality() == pdata.MetricAggregationTemporalityCumulative
	case pdata.MetricDataTypeSummary:
		return metric.Summary().DataPoints().Len() != 0
	}
	return false
}

// addSingleNumberDataPoint converts the metric value stored in pt to a Prometheus sample, and add the sample
// to its corresponding time series in tsMap
func addSingleNumberDataPoint(pt pdata.NumberDataPoint, resource pdata.Resource, metric pdata.Metric, settings Settings, tsMap map[string]*prompb.TimeSeries) {
	// create parameters for addSample
	name := getPromMetricName(metric, settings.Namespace)
	labels := createAttributes(resource, pt.Attributes(), settings.ExternalLabels, nameStr, name)
	sample := &prompb.Sample{
		// convert ns to ms
		Timestamp: convertTimeStamp(pt.Timestamp()),
	}
	switch pt.ValueType() {
	case pdata.MetricValueTypeInt:
		sample.Value = float64(pt.IntVal())
	case pdata.MetricValueTypeDouble:
		sample.Value = pt.DoubleVal()
	}
	if pt.Flags().HasFlag(pdata.MetricDataPointFlagNoRecordedValue) {
		sample.Value = math.Float64frombits(value.StaleNaN)
	}
	addSample(tsMap, sample, labels, metric)
}

// addSingleHistogramDataPoint converts pt to 2 + min(len(ExplicitBounds), len(BucketCount)) + 1 samples. It
// ignore extra buckets if len(ExplicitBounds) > len(BucketCounts)
func addSingleHistogramDataPoint(pt pdata.HistogramDataPoint, resource pdata.Resource, metric pdata.Metric, settings Settings, tsMap map[string]*prompb.TimeSeries) {
	time := convertTimeStamp(pt.Timestamp())
	// sum, count, and buckets of the histogram should append suffix to baseName
	baseName := getPromMetricName(metric, settings.Namespace)
	// treat sum as a sample in an individual TimeSeries
	sum := &prompb.Sample{
		Value:     pt.Sum(),
		Timestamp: time,
	}
	if pt.Flags().HasFlag(pdata.MetricDataPointFlagNoRecordedValue) {
		sum.Value = math.Float64frombits(value.StaleNaN)
	}

	sumlabels := createAttributes(resource, pt.Attributes(), settings.ExternalLabels, nameStr, baseName+sumStr)
	addSample(tsMap, sum, sumlabels, metric)

	// treat count as a sample in an individual TimeSeries
	count := &prompb.Sample{
		Value:     float64(pt.Count()),
		Timestamp: time,
	}
	if pt.Flags().HasFlag(pdata.MetricDataPointFlagNoRecordedValue) {
		count.Value = math.Float64frombits(value.StaleNaN)
	}

	countlabels := createAttributes(resource, pt.Attributes(), settings.ExternalLabels, nameStr, baseName+countStr)
	addSample(tsMap, count, countlabels, metric)

	// cumulative count for conversion to cumulative histogram
	var cumulativeCount uint64

	promExemplars := getPromExemplars(pt)

	bucketBounds := make([]bucketBoundsData, 0)

	// process each bound, based on histograms proto definition, # of buckets = # of explicit bounds + 1
	for index, bound := range pt.ExplicitBounds() {
		if index >= len(pt.BucketCounts()) {
			break
		}
		cumulativeCount += pt.BucketCounts()[index]
		bucket := &prompb.Sample{
			Value:     float64(cumulativeCount),
			Timestamp: time,
		}
		if pt.Flags().HasFlag(pdata.MetricDataPointFlagNoRecordedValue) {
			bucket.Value = math.Float64frombits(value.StaleNaN)
		}
		boundStr := strconv.FormatFloat(bound, 'f', -1, 64)
		labels := createAttributes(resource, pt.Attributes(), settings.ExternalLabels, nameStr, baseName+bucketStr, leStr, boundStr)
		sig := addSample(tsMap, bucket, labels, metric)

		bucketBounds = append(bucketBounds, bucketBoundsData{sig: sig, bound: bound})
	}
	// add le=+Inf bucket
	infBucket := &prompb.Sample{
		Timestamp: time,
	}
	if pt.Flags().HasFlag(pdata.MetricDataPointFlagNoRecordedValue) {
		infBucket.Value = math.Float64frombits(value.StaleNaN)
	} else {
		cumulativeCount += pt.BucketCounts()[len(pt.BucketCounts())-1]
		infBucket.Value = float64(cumulativeCount)
	}
	infLabels := createAttributes(resource, pt.Attributes(), settings.ExternalLabels, nameStr, baseName+bucketStr, leStr, pInfStr)
	sig := addSample(tsMap, infBucket, infLabels, metric)

	bucketBounds = append(bucketBounds, bucketBoundsData{sig: sig, bound: math.Inf(1)})
	addExemplars(tsMap, promExemplars, bucketBounds)
}

func getPromExemplars(pt pdata.HistogramDataPoint) []prompb.Exemplar {
	var promExemplars []prompb.Exemplar

	for i := 0; i < pt.Exemplars().Len(); i++ {
		exemplar := pt.Exemplars().At(i)

		promExemplar := &prompb.Exemplar{
			Value:     exemplar.DoubleVal(),
			Timestamp: timestamp.FromTime(exemplar.Timestamp().AsTime()),
		}

		exemplar.FilteredAttributes().Range(func(key string, value pdata.AttributeValue) bool {
			promLabel := prompb.Label{
				Name:  key,
				Value: value.AsString(),
			}

			promExemplar.Labels = append(promExemplar.Labels, promLabel)

			return true
		})

		promExemplars = append(promExemplars, *promExemplar)
	}

	return promExemplars
}

// addSingleSummaryDataPoint converts pt to len(QuantileValues) + 2 samples.
func addSingleSummaryDataPoint(pt pdata.SummaryDataPoint, resource pdata.Resource, metric pdata.Metric, settings Settings,
	tsMap map[string]*prompb.TimeSeries) {
	time := convertTimeStamp(pt.Timestamp())
	// sum and count of the summary should append suffix to baseName
	baseName := getPromMetricName(metric, settings.Namespace)
	// treat sum as a sample in an individual TimeSeries
	sum := &prompb.Sample{
		Value:     pt.Sum(),
		Timestamp: time,
	}
	if pt.Flags().HasFlag(pdata.MetricDataPointFlagNoRecordedValue) {
		sum.Value = math.Float64frombits(value.StaleNaN)
	}
	sumlabels := createAttributes(resource, pt.Attributes(), settings.ExternalLabels, nameStr, baseName+sumStr)
	addSample(tsMap, sum, sumlabels, metric)

	// treat count as a sample in an individual TimeSeries
	count := &prompb.Sample{
		Value:     float64(pt.Count()),
		Timestamp: time,
	}
	if pt.Flags().HasFlag(pdata.MetricDataPointFlagNoRecordedValue) {
		count.Value = math.Float64frombits(value.StaleNaN)
	}
	countlabels := createAttributes(resource, pt.Attributes(), settings.ExternalLabels, nameStr, baseName+countStr)
	addSample(tsMap, count, countlabels, metric)

	// process each percentile/quantile
	for i := 0; i < pt.QuantileValues().Len(); i++ {
		qt := pt.QuantileValues().At(i)
		quantile := &prompb.Sample{
			Value:     qt.Value(),
			Timestamp: time,
		}
		if pt.Flags().HasFlag(pdata.MetricDataPointFlagNoRecordedValue) {
			quantile.Value = math.Float64frombits(value.StaleNaN)
		}
		percentileStr := strconv.FormatFloat(qt.Quantile(), 'f', -1, 64)
		qtlabels := createAttributes(resource, pt.Attributes(), settings.ExternalLabels, nameStr, baseName, quantileStr, percentileStr)
		addSample(tsMap, quantile, qtlabels, metric)
	}
}

// copied from prometheus-go-metric-exporter
// sanitize replaces non-alphanumeric characters with underscores in s.
func sanitize(s string) string {
	if len(s) == 0 {
		return s
	}

	// Note: No length limit for label keys because Prometheus doesn't
	// define a length limit, thus we should NOT be truncating label keys.
	// See https://github.com/orijtech/prometheus-go-metrics-exporter/issues/4.
	s = strings.Map(sanitizeRune, s)
	if unicode.IsDigit(rune(s[0])) {
		s = keyStr + "_" + s
	}
	if s[0] == '_' {
		s = keyStr + s
	}
	return s
}

// copied from prometheus-go-metric-exporter
// sanitizeRune converts anything that is not a letter or digit to an underscore
func sanitizeRune(r rune) rune {
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return r
	}
	// Everything else turns into an underscore
	return '_'
}

// convertTimeStamp converts OTLP timestamp in ns to timestamp in ms
func convertTimeStamp(timestamp pdata.Timestamp) int64 {
	return timestamp.AsTime().UnixNano() / (int64(time.Millisecond) / int64(time.Nanosecond))
}
