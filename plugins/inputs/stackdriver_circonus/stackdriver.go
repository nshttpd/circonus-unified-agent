package stackdriver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3"
	"github.com/circonus-labs/circonus-unified-agent/cua"
	"github.com/circonus-labs/circonus-unified-agent/internal"
	circmgr "github.com/circonus-labs/circonus-unified-agent/internal/circonus"
	"github.com/circonus-labs/circonus-unified-agent/internal/limiter"
	cuametric "github.com/circonus-labs/circonus-unified-agent/metric"
	"github.com/circonus-labs/circonus-unified-agent/plugins/inputs" // Imports the Stackdriver Monitoring client package.
	"github.com/circonus-labs/circonus-unified-agent/selfstat"
	googlepbduration "github.com/golang/protobuf/ptypes/duration"
	googlepbts "github.com/golang/protobuf/ptypes/timestamp"
	"google.golang.org/api/iterator"
	distributionpb "google.golang.org/genproto/googleapis/api/distribution"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	monitoringpb "google.golang.org/genproto/googleapis/monitoring/v3"
)

const (
	defaultRateLimit = 14
	description      = "Gather timeseries from Google Cloud Platform v3 monitoring API"
	sampleConfig     = `
  # Instance ID is required
  instance_id = ""

  ## GCP Project
  project = "erudite-bloom-151019"

  ## Many metrics are updated once per minute; it is recommended to override
  ## the agent level interval with a value of 1m or greater.
  interval = "1m"

  ## Maximum number of API calls to make per second.  The quota for accounts
  ## varies, it can be viewed on the API dashboard:
  ##   https://cloud.google.com/monitoring/quotas#quotas_and_limits
  # rate_limit = 14

  ## The delay and window options control the number of points selected on
  ## each gather.  When set, metrics are gathered between:
  ##   start: now() - delay - window
  ##   end:   now() - delay
  #
  ## Collection delay; if set too low metrics may not yet be available.
  # delay = "5m"
  #
  ## If unset, the window will start at 1m and be updated dynamically to span
  ## the time between calls (approximately the length of the plugin interval).
  # window = "1m"

  ## TTL for cached list of metric types.  This is the maximum amount of time
  ## it may take to discover new metrics.
  # cache_ttl = "1h"

  ## If true, raw bucket counts are collected for distribution value types.
  ## For a more lightweight collection, you may wish to disable and use
  ## distribution_aggregation_aligners instead.
  # gather_raw_distribution_buckets = true

  ## Aggregate functions to be used for metrics whose value type is
  ## distribution.  These aggregate values are recorded in in addition to raw
  ## bucket counts; if they are enabled.
  ##
  ## For a list of aligner strings see:
  ##   https://cloud.google.com/monitoring/api/ref_v3/rpc/google.monitoring.v3#aligner
  # distribution_aggregation_aligners = [
  # 	"ALIGN_PERCENTILE_99",
  # 	"ALIGN_PERCENTILE_95",
  # 	"ALIGN_PERCENTILE_50",
  # ]

  ## Filters can be added to reduce the number of time series matched.  All
  ## functions are supported: starts_with, ends_with, has_substring, and
  ## one_of.  Only the '=' operator is supported.
  ##
  ## The logical operators when combining filters are defined statically using
  ## the following values:
  ##   filter ::= <resource_labels> {AND <metric_labels>}
  ##   resource_labels ::= <resource_labels> {OR <resource_label>}
  ##   metric_labels ::= <metric_labels> {OR <metric_label>}
  ##
  ## For more details, see https://cloud.google.com/monitoring/api/v3/filters
  #
  ## Resource labels refine the time series selection with the following expression:
  ##   resource.labels.<key> = <value>
  # [[inputs.stackdriver.filter.resource_labels]]
  #   key = "instance_name"
  #   value = 'starts_with("localhost")'
  #
  ## Metric labels refine the time series selection with the following expression:
  ##   metric.labels.<key> = <value>
  #  [[inputs.stackdriver.filter.metric_labels]]
  #  	 key = "device_name"
  #  	 value = 'one_of("sda", "sdb")'
`
)

var (
	defaultCacheTTL = internal.Duration{Duration: 1 * time.Hour}
	defaultWindow   = internal.Duration{Duration: 1 * time.Minute}
	defaultDelay    = internal.Duration{Duration: 5 * time.Minute}
)

// Stackdriver is the Google Stackdriver config info.
type Stackdriver struct {
	prevEnd                         time.Time
	client                          metricClient
	Log                             cua.Logger
	timeSeriesConfCache             *timeSeriesConfCache
	Filter                          *ListTimeSeriesFilter `toml:"filter"`
	Project                         string                `toml:"project"`
	MetricTypePrefixExclude         []string
	MetricTypePrefixInclude         []string
	DistributionAggregationAligners []string          `toml:"distribution_aggregation_aligners"`
	CacheTTL                        internal.Duration `toml:"cache_ttl"`
	Delay                           internal.Duration `toml:"delay"`
	Window                          internal.Duration `toml:"window"`
	RateLimit                       int               `toml:"rate_limit"`
	GatherRawDistributionBuckets    bool              `toml:"gather_raw_distribution_buckets"`
}

// ListTimeSeriesFilter contains resource labels and metric labels
type ListTimeSeriesFilter struct {
	ResourceLabels []*Label `json:"resource_labels"`
	MetricLabels   []*Label `json:"metric_labels"`
}

// Label contains key and value
type Label struct {
	Key   string `toml:"key"`
	Value string `toml:"value"`
}

// TimeSeriesConfCache caches generated timeseries configurations
type timeSeriesConfCache struct {
	Generated       time.Time
	TimeSeriesConfs []*timeSeriesConf
	TTL             time.Duration
}

// Internal structure which holds our configuration for a particular GCP time series.
type timeSeriesConf struct {
	// The GCP API request that we'll use to fetch data for this time series.
	listTimeSeriesRequest *monitoringpb.ListTimeSeriesRequest
	// The influx measurement name this time series maps to
	measurement string
	// The prefix to use before any influx field names that we'll write for
	// this time series. (Or, if we only decide to write one field name, this
	// field just holds the value of the field name.)
	fieldKey string
}

// stackdriverMetricClient is a metric client for stackdriver
type stackdriverMetricClient struct {
	log  cua.Logger
	conn *monitoring.MetricClient

	listMetricDescriptorsCalls selfstat.Stat
	listTimeSeriesCalls        selfstat.Stat
}

// metricClient is convenient for testing
type metricClient interface {
	ListMetricDescriptors(ctx context.Context, req *monitoringpb.ListMetricDescriptorsRequest) (<-chan *metricpb.MetricDescriptor, error)
	ListTimeSeries(ctx context.Context, req *monitoringpb.ListTimeSeriesRequest) (<-chan *monitoringpb.TimeSeries, error)
	Close() error
}

type lockedSeriesGrouper struct {
	sync.Mutex
	*cuametric.SeriesGrouper
}

func (g *lockedSeriesGrouper) Add(
	measurement string,
	tags map[string]string,
	tm time.Time,
	field string,
	fieldValue interface{},
) error {
	g.Lock()
	defer g.Unlock()
	return g.SeriesGrouper.Add(measurement, tags, tm, field, fieldValue)
}

// ListMetricDescriptors implements metricClient interface
func (smc *stackdriverMetricClient) ListMetricDescriptors(
	ctx context.Context,
	req *monitoringpb.ListMetricDescriptorsRequest,
) (<-chan *metricpb.MetricDescriptor, error) {
	mdChan := make(chan *metricpb.MetricDescriptor, 1000)

	go func() {
		// smc.log.Debugf("List metric descriptor request filter: %s", req.Filter)
		defer close(mdChan)

		// Iterate over metric descriptors and send them to buffered channel
		mdResp := smc.conn.ListMetricDescriptors(ctx, req)
		smc.listMetricDescriptorsCalls.Incr(1)
		for {
			if smc.done(ctx) {
				return
			}
			mdDesc, mdErr := mdResp.Next()
			if mdErr != nil {
				if !errors.Is(mdErr, iterator.Done) {
					smc.log.Errorf("Failed iterating metric descriptor responses: %q: %v", req.String(), mdErr)
				}
				break
			}
			mdChan <- mdDesc
		}
	}()

	return mdChan, nil
}

// ListTimeSeries implements metricClient interface
func (smc *stackdriverMetricClient) ListTimeSeries(
	ctx context.Context,
	req *monitoringpb.ListTimeSeriesRequest,
) (<-chan *monitoringpb.TimeSeries, error) {
	tsChan := make(chan *monitoringpb.TimeSeries, 1000)

	go func() {
		// smc.log.Debugf("List time series request filter: %s", req.Filter)
		defer close(tsChan)

		// Iterate over timeseries and send them to buffered channel
		tsResp := smc.conn.ListTimeSeries(ctx, req)
		smc.listTimeSeriesCalls.Incr(1)
		for {
			if smc.done(ctx) {
				return
			}
			tsDesc, tsErr := tsResp.Next()
			if tsErr != nil {
				if !errors.Is(tsErr, iterator.Done) {
					smc.log.Errorf("Failed iterating time series responses: %q: %v", req.String(), tsErr)
				}
				break
			}
			tsChan <- tsDesc
		}
	}()

	return tsChan, nil
}

// Close implements metricClient interface
func (smc *stackdriverMetricClient) Close() error {
	return smc.conn.Close()
}

func (smc *stackdriverMetricClient) done(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// Description implements cua.Input interface
func (s *Stackdriver) Description() string {
	return description
}

// SampleConfig implements cua.Input interface
func (s *Stackdriver) SampleConfig() string {
	return sampleConfig
}

// Gather implements cua.Input interface
func (s *Stackdriver) Gather(ctx context.Context, acc cua.Accumulator) error {

	if s.RateLimit == 0 {
		s.RateLimit = defaultRateLimit
	}

	err := s.initializeStackdriverClient(ctx)
	if err != nil {
		return err
	}

	start, end := s.updateWindow(s.prevEnd)
	s.prevEnd = end

	tsConfs, err := s.generatetimeSeriesConfs(ctx, start, end)
	if err != nil {
		return err
	}

	lmtr := limiter.NewRateLimiter(s.RateLimit, time.Second)
	defer lmtr.Stop()

	grouper := &lockedSeriesGrouper{
		SeriesGrouper: cuametric.NewSeriesGrouper(),
	}

	var wg sync.WaitGroup
	wg.Add(len(tsConfs))
	for _, tsConf := range tsConfs {
		<-lmtr.C
		go func(tsConf *timeSeriesConf) {
			defer wg.Done()
			acc.AddError(s.gatherTimeSeries(ctx, grouper, tsConf, acc))
		}(tsConf)
	}
	wg.Wait()

	for _, metric := range grouper.Metrics() {
		acc.AddMetric(metric)
	}

	return nil
}

// Returns the start and end time for the next collection.
func (s *Stackdriver) updateWindow(prevEnd time.Time) (time.Time, time.Time) {
	var start time.Time
	switch {
	case s.Window.Duration != 0:
		start = time.Now().Add(-s.Delay.Duration).Add(-s.Window.Duration)
	case prevEnd.IsZero():
		start = time.Now().Add(-s.Delay.Duration).Add(-defaultWindow.Duration)
	default:
		start = prevEnd
	}
	end := time.Now().Add(-s.Delay.Duration)
	return start, end
}

// Generate filter string for ListTimeSeriesRequest
func (s *Stackdriver) newListTimeSeriesFilter(metricType string) string {
	functions := []string{
		"starts_with",
		"ends_with",
		"has_substring",
		"one_of",
	}
	filterString := fmt.Sprintf(`metric.type = "%s"`, metricType)
	if s.Filter == nil {
		return filterString
	}

	var valueFmt string
	if len(s.Filter.ResourceLabels) > 0 {
		resourceLabelsFilter := make([]string, len(s.Filter.ResourceLabels))
		for i, resourceLabel := range s.Filter.ResourceLabels {
			// check if resource label value contains function
			if includeExcludeHelper(resourceLabel.Value, functions, nil) {
				valueFmt = `resource.labels.%s = %s`
			} else {
				valueFmt = `resource.labels.%s = "%s"`
			}
			resourceLabelsFilter[i] = fmt.Sprintf(valueFmt, resourceLabel.Key, resourceLabel.Value)
		}
		if len(resourceLabelsFilter) == 1 {
			filterString += fmt.Sprintf(" AND %s", resourceLabelsFilter[0])
		} else {
			filterString += fmt.Sprintf(" AND (%s)", strings.Join(resourceLabelsFilter, " OR "))
		}
	}

	if len(s.Filter.MetricLabels) > 0 {
		metricLabelsFilter := make([]string, len(s.Filter.MetricLabels))
		for i, metricLabel := range s.Filter.MetricLabels {
			// check if metric label value contains function
			if includeExcludeHelper(metricLabel.Value, functions, nil) {
				valueFmt = `metric.labels.%s = %s`
			} else {
				valueFmt = `metric.labels.%s = "%s"`
			}
			metricLabelsFilter[i] = fmt.Sprintf(valueFmt, metricLabel.Key, metricLabel.Value)
		}
		if len(metricLabelsFilter) == 1 {
			filterString += fmt.Sprintf(" AND %s", metricLabelsFilter[0])
		} else {
			filterString += fmt.Sprintf(" AND (%s)", strings.Join(metricLabelsFilter, " OR "))
		}
	}

	return filterString
}

// Create and initialize a timeSeriesConf for a given GCP metric type with
// defaults taken from the gcp_stackdriver plugin configuration.
func (s *Stackdriver) newTimeSeriesConf(
	metricType string, startTime, endTime time.Time,
) *timeSeriesConf {
	filter := s.newListTimeSeriesFilter(metricType)
	interval := &monitoringpb.TimeInterval{
		EndTime:   &googlepbts.Timestamp{Seconds: endTime.Unix()},
		StartTime: &googlepbts.Timestamp{Seconds: startTime.Unix()},
	}
	tsReq := &monitoringpb.ListTimeSeriesRequest{
		Name:     fmt.Sprintf("projects/%s", s.Project),
		Filter:   filter,
		Interval: interval,
	}
	cfg := &timeSeriesConf{
		measurement:           metricType,
		fieldKey:              "value",
		listTimeSeriesRequest: tsReq,
	}

	// GCP metric types have at least one slash, but we'll be defensive anyway.
	slashIdx := strings.LastIndex(metricType, "/")
	if slashIdx > 0 {
		cfg.measurement = metricType[:slashIdx]
		cfg.fieldKey = metricType[slashIdx+1:]
	}

	return cfg
}

// Change this configuration to query an aggregate by specifying an "aligner".
// In GCP monitoring, "aligning" is aggregation performed *within* a time
// series, to distill a pile of data points down to a single data point for
// some given time period (here, we specify 60s as our time period). This is
// especially useful for scraping GCP "distribution" metric types, whose raw
// data amounts to a ~60 bucket histogram, which is fairly hard to query and
// visualize in the TICK stack.
func (t *timeSeriesConf) initForAggregate(alignerStr string) {
	// Check if alignerStr is valid
	alignerInt, isValid := monitoringpb.Aggregation_Aligner_value[alignerStr]
	if !isValid {
		alignerStr = monitoringpb.Aggregation_Aligner_name[alignerInt]
	}
	aligner := monitoringpb.Aggregation_Aligner(alignerInt)
	agg := &monitoringpb.Aggregation{
		AlignmentPeriod:  &googlepbduration.Duration{Seconds: 60},
		PerSeriesAligner: aligner,
	}
	t.fieldKey = t.fieldKey + "_" + strings.ToLower(alignerStr)
	t.listTimeSeriesRequest.Aggregation = agg
}

// IsValid checks timeseriesconf cache validity
func (c *timeSeriesConfCache) IsValid() bool {
	return c.TimeSeriesConfs != nil && time.Since(c.Generated) < c.TTL
}

func (s *Stackdriver) initializeStackdriverClient(ctx context.Context) error {
	if s.client == nil {
		client, err := monitoring.NewMetricClient(ctx)
		if err != nil {
			return fmt.Errorf("failed to create stackdriver monitoring client: %w", err)
		}

		tags := map[string]string{
			"project_id": s.Project,
		}
		listMetricDescriptorsCalls := selfstat.Register(
			"stackdriver", "list_metric_descriptors_calls", tags)
		listTimeSeriesCalls := selfstat.Register(
			"stackdriver", "list_timeseries_calls", tags)

		s.client = &stackdriverMetricClient{
			log:                        s.Log,
			conn:                       client,
			listMetricDescriptorsCalls: listMetricDescriptorsCalls,
			listTimeSeriesCalls:        listTimeSeriesCalls,
		}
	}

	return nil
}

func includeExcludeHelper(key string, includes []string, excludes []string) bool {
	if len(includes) > 0 {
		for _, includeStr := range includes {
			if strings.HasPrefix(key, includeStr) {
				return true
			}
		}
		return false
	}
	if len(excludes) > 0 {
		for _, excludeStr := range excludes {
			if strings.HasPrefix(key, excludeStr) {
				return false
			}
		}
		return true
	}
	return true
}

// Test whether a particular GCP metric type should be scraped by this plugin
// by checking the plugin name against the configuration's
// "includeMetricTypePrefixes" and "excludeMetricTypePrefixes"
func (s *Stackdriver) includeMetricType(metricType string) bool {
	k := metricType
	inc := s.MetricTypePrefixInclude
	exc := s.MetricTypePrefixExclude

	return includeExcludeHelper(k, inc, exc)
}

// Generates filter for list metric descriptors request
func (s *Stackdriver) newListMetricDescriptorsFilters() []string {
	if len(s.MetricTypePrefixInclude) == 0 {
		return nil
	}

	metricTypeFilters := make([]string, len(s.MetricTypePrefixInclude))
	for i, metricTypePrefix := range s.MetricTypePrefixInclude {
		metricTypeFilters[i] = fmt.Sprintf(`metric.type = starts_with(%q)`, metricTypePrefix)
	}
	return metricTypeFilters
}

// Generate a list of timeSeriesConfig structs by making a ListMetricDescriptors
// API request and filtering the result against our configuration.
func (s *Stackdriver) generatetimeSeriesConfs(
	ctx context.Context, startTime, endTime time.Time,
) ([]*timeSeriesConf, error) {
	if s.timeSeriesConfCache != nil && s.timeSeriesConfCache.IsValid() {
		// Update interval for timeseries requests in timeseries cache
		interval := &monitoringpb.TimeInterval{
			EndTime:   &googlepbts.Timestamp{Seconds: endTime.Unix()},
			StartTime: &googlepbts.Timestamp{Seconds: startTime.Unix()},
		}
		for _, timeSeriesConf := range s.timeSeriesConfCache.TimeSeriesConfs {
			timeSeriesConf.listTimeSeriesRequest.Interval = interval
		}
		return s.timeSeriesConfCache.TimeSeriesConfs, nil
	}

	ret := []*timeSeriesConf{}
	req := &monitoringpb.ListMetricDescriptorsRequest{
		Name: fmt.Sprintf("projects/%s", s.Project),
	}

	filters := s.newListMetricDescriptorsFilters()
	if len(filters) == 0 {
		filters = []string{""}
	}

	for _, filter := range filters {
		// Add filter for list metric descriptors if
		// includeMetricTypePrefixes is specified,
		// this is more efficient than iterating over
		// all metric descriptors
		req.Filter = filter
		mdRespChan, err := s.client.ListMetricDescriptors(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("list metric descriptors: %w", err)
		}

		for metricDescriptor := range mdRespChan {
			metricType := metricDescriptor.Type
			valueType := metricDescriptor.ValueType

			if filter == "" && !s.includeMetricType(metricType) {
				continue
			}

			if valueType == metricpb.MetricDescriptor_DISTRIBUTION {
				if s.GatherRawDistributionBuckets {
					tsConf := s.newTimeSeriesConf(metricType, startTime, endTime)
					ret = append(ret, tsConf)
				}
				for _, alignerStr := range s.DistributionAggregationAligners {
					tsConf := s.newTimeSeriesConf(metricType, startTime, endTime)
					tsConf.initForAggregate(alignerStr)
					ret = append(ret, tsConf)
				}
			} else {
				ret = append(ret, s.newTimeSeriesConf(metricType, startTime, endTime))
			}
			if s.done(ctx) {
				break
			}
		}
		if s.done(ctx) {
			break
		}
	}

	s.timeSeriesConfCache = &timeSeriesConfCache{
		TimeSeriesConfs: ret,
		Generated:       time.Now(),
		TTL:             s.CacheTTL.Duration,
	}

	return ret, nil
}

// Do the work to gather an individual time series. Runs inside a
// timeseries-specific goroutine.
func (s *Stackdriver) gatherTimeSeries(
	ctx context.Context, grouper *lockedSeriesGrouper, tsConf *timeSeriesConf, acc cua.Accumulator,
) error {
	tsReq := tsConf.listTimeSeriesRequest

	tsRespChan, err := s.client.ListTimeSeries(ctx, tsReq)
	if err != nil {
		return fmt.Errorf("list time series: %w", err)
	}

	for tsDesc := range tsRespChan {
		tags := map[string]string{
			"resource_type": tsDesc.Resource.Type,
			"project_id":    s.Project,
		}
		for k, v := range tsDesc.Resource.Labels {
			tags[k] = v
		}
		for k, v := range tsDesc.Metric.Labels {
			tags[k] = v
		}

		// add metric category to prevent collisions
		slashIdx := strings.LastIndex(tsConf.measurement, "/")
		if slashIdx > 0 {
			cat := tsConf.measurement[slashIdx+1:]
			tags["metric_category"] = cat
		} else {
			tags["metric_category"] = tsConf.measurement
		}

		// add stackdriver metrickind as a tag so proper math can applied ex-post facto
		switch tsDesc.MetricKind {
		case metricpb.MetricDescriptor_METRIC_KIND_UNSPECIFIED:
			tags["metric_kind"] = "unspecified"
		case metricpb.MetricDescriptor_GAUGE:
			tags["metric_kind"] = "gauge"
		case metricpb.MetricDescriptor_DELTA:
			tags["metric_kind"] = "delta"
		case metricpb.MetricDescriptor_CUMULATIVE:
			tags["metric_kind"] = "cumulative"
		}

		// s.Log.Debugf("%s %v %v\n", tsConf.fieldKey, tags, tsDesc.ValueType)

		for _, p := range tsDesc.Points {
			ts := time.Unix(p.Interval.EndTime.Seconds, 0)

			if tsDesc.ValueType == metricpb.MetricDescriptor_DISTRIBUTION {
				dist := p.Value.GetDistributionValue()

				// s.Log.Debugf("DISTRIBUTION: %s %v %v\n", tsConf.fieldKey, tags, dist)
				s.addDistribution(dist, tags, ts, grouper, tsConf, acc, tsDesc.MetricKind)
			} else {
				var value interface{}

				// Types that are valid to be assigned to Value
				// See: https://godoc.org/google.golang.org/genproto/googleapis/monitoring/v3#TypedValue
				switch tsDesc.ValueType {
				case metricpb.MetricDescriptor_BOOL:
					value = 0
					v := p.Value.GetBoolValue()
					if v {
						value = 1
					}
				case metricpb.MetricDescriptor_INT64:
					value = p.Value.GetInt64Value()
				case metricpb.MetricDescriptor_DOUBLE:
					value = p.Value.GetDoubleValue()
				case metricpb.MetricDescriptor_STRING:
					value = p.Value.GetStringValue()
				}

				_ = grouper.Add(tsConf.measurement, tags, ts, tsConf.fieldKey, value)
			}
			if s.done(ctx) {
				break
			}
		}
		if s.done(ctx) {
			break
		}
	}

	return nil
}

func distributionToCircHisto(s *Stackdriver, //nolint:unparam
	metric *distributionpb.Distribution,
	options *distributionpb.Distribution_BucketOptions) map[string]int64 {

	linearBuckets := options.GetLinearBuckets()
	exponentialBuckets := options.GetExponentialBuckets()
	explicitBuckets := options.GetExplicitBuckets()

	ret := make(map[string]int64)

	var numBuckets int32
	switch {
	case linearBuckets != nil:
		numBuckets = linearBuckets.NumFiniteBuckets + 2
	case exponentialBuckets != nil:
		numBuckets = exponentialBuckets.NumFiniteBuckets + 2
	default:
		numBuckets = int32(len(explicitBuckets.Bounds)) + 1
	}

	// s.Log.Debugf("numBuckets: %d\n", numBuckets)
	// s.Log.Debugf("dist2circhist BucketCounts: %d, Count: %d\n", len(metric.BucketCounts), metric.Count)

	var i int32
	var count int64
	for i = 0; i < numBuckets; i++ {
		// The last bucket is the overflow bucket, and includes all values
		// greater than the previous bound.
		var localCount int64 = 0
		// Add to the cumulative count; trailing buckets with value 0 are
		// omitted from the response.
		if i < int32(len(metric.BucketCounts)) {
			localCount = metric.BucketCounts[i]
			count += localCount
			// s.Log.Debugf("Bucket %d count: %d\n", i, localCount)
		}

		if localCount > 0 {
			var upperBound float64
			switch {
			case i == 0:
				upperBound = 0
			case i == numBuckets-1:
				upperBound = 10e+127
			case linearBuckets != nil:
				upperBound = linearBuckets.Offset + (linearBuckets.Width * float64(i))
			case exponentialBuckets != nil:
				width := math.Pow(exponentialBuckets.GrowthFactor, float64(i))
				upperBound = exponentialBuckets.Scale * width
			case explicitBuckets != nil:
				upperBound = explicitBuckets.Bounds[i]
			}
			// s.Log.Debugf("Adding bucket H[%e]=%d\n", upperBound, localCount)
			ret[fmt.Sprintf("%e", upperBound)] = localCount
		}
	}

	return ret
}

// AddDistribution adds metrics from a distribution value type.
func (s *Stackdriver) addDistribution(
	metric *distributionpb.Distribution,
	tags map[string]string, ts time.Time,
	grouper *lockedSeriesGrouper, tsConf *timeSeriesConf,
	acc cua.Accumulator, metricKind metricpb.MetricDescriptor_MetricKind,
) {
	field := tsConf.fieldKey
	name := tsConf.measurement

	_ = grouper.Add(name, tags, ts, field+"_count", metric.Count)
	_ = grouper.Add(name, tags, ts, field+"_mean", metric.Mean)
	_ = grouper.Add(name, tags, ts, field+"_sum_of_squared_deviation", metric.SumOfSquaredDeviation)

	if metric.Range != nil {
		_ = grouper.Add(name, tags, ts, field+"_range_min", metric.Range.Min)
		_ = grouper.Add(name, tags, ts, field+"_range_max", metric.Range.Max)
	}

	circhisto := distributionToCircHisto(s, metric, metric.BucketOptions)

	// add the metric prefix as the metric group so
	// these metrics will go to the correct check
	// unlike other metrics the "fields" of a histogram
	// are the buckets, not the discrete metrics
	newTags := make(map[string]string)
	for k, v := range tags {
		newTags[k] = v
	}
	newTags["input_metric_group"] = name

	if len(circhisto) > 0 {
		// s.Log.Debugf("Histogram has %d buckets\n", len(circhisto))
		var histometric cua.Metric = nil
		for key, value := range circhisto {
			if histometric == nil {
				mk := cua.Histogram
				if metricKind == metricpb.MetricDescriptor_CUMULATIVE {
					mk = cua.CumulativeHistogram
				}
				// histometric, _ = cuametric.New(field, tags, map[string]interface{}{key: value}, ts, mk)
				histometric, _ = cuametric.New(field, newTags, map[string]interface{}{key: value}, ts, mk)
			} else {
				histometric.AddField(key, value)
			}
		}
		acc.AddMetric(histometric)
	} /*else {
		s.Log.Debugf("Histogram has 0 buckets\n")
	}*/
}

func (s *Stackdriver) done(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func init() {
	f := func() cua.Input {
		return &Stackdriver{
			CacheTTL:                        defaultCacheTTL,
			RateLimit:                       defaultRateLimit,
			Delay:                           defaultDelay,
			MetricTypePrefixInclude:         circmgr.GCPMetricTypePrefixInclude(),
			MetricTypePrefixExclude:         []string{},
			GatherRawDistributionBuckets:    true,
			DistributionAggregationAligners: []string{},
		}
	}

	inputs.Add("stackdriver_circonus", f)
}
