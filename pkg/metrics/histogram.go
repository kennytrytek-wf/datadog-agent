// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package metrics

import (
	"fmt"
	"math"
	"sort"
	"strconv"

	pkgconfigmodel "github.com/DataDog/datadog-agent/pkg/config/model"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

// weightSample represent a sample with its weight in the histogram (deduce from SampleRate)
type weightSample struct {
	value  float64
	weight int64
}

type weightSamples []weightSample

func (w weightSamples) Len() int           { return len(w) }
func (w weightSamples) Less(i, j int) bool { return w[i].value < w[j].value }
func (w weightSamples) Swap(i, j int)      { w[i], w[j] = w[j], w[i] }

// Histogram tracks the distribution of samples added over one flush period
type Histogram struct {
	aggregates  []string // aggregates configured on this histogram
	percentiles []int    // percentiles configured on this histogram, each in the 1-100 range
	interval    int64    // interval over which the `count` value is normalized (bucket interval for Dogstatsd, 1 otherwise)
	samples     weightSamples
	count       int64
	unit        string // unit carried by samples (e.g. "millisecond" for timing metrics)
	// sharedWeight enables a cheaper sum path at flush:
	//   0  -> no samples yet
	//   >0 -> every sample so far has this weight (sorted-by-value order matches sorted-by-term order)
	//   -1 -> weights vary; must use the general magnitude-compensating sum
	sharedWeight int64
}

const (
	maxAgg    = "max"
	minAgg    = "min"
	medianAgg = "median"
	avgAgg    = "avg"
	sumAgg    = "sum"
	countAgg  = "count"
)

var (
	defaultAggregates  = []string(nil)
	defaultPercentiles = []int(nil)
)

// ParsePercentiles represents a string percentile in
// an integer percentile (e.g. "0.95" -> 95, "0.85" -> 85
func ParsePercentiles(percentiles []string) []int {
	res := []int{}
	for _, p := range percentiles {
		i, err := strconv.ParseFloat(p, 64)
		if err != nil {
			log.Errorf("Could not parse '%s' from 'histogram_percentiles' (skipping): %s", p, err)
			continue
		}
		if i < 0 || i > 1 {
			log.Errorf("histogram_percentiles must be between 0 and 1: skipping %f", i)
			continue
		}
		// in some cases the '*100' will lower the number resulting in
		// an int lower by 1 from what is expected (ex: 0.29 would
		// become 28). As a workaround we add 0.5 before casting.
		res = append(res, int(i*100+0.5))
	}
	return res
}

// NewHistogram returns a newly initialized histogram
func NewHistogram(interval int64, config pkgconfigmodel.Config) *Histogram {
	// we initialize default value on the first histogram creation
	if defaultAggregates == nil {
		defaultAggregates = config.GetStringSlice("histogram_aggregates")
	}

	if defaultPercentiles == nil {
		c := config.GetStringSlice("histogram_percentiles")
		defaultPercentiles = ParsePercentiles(c)
		sort.Ints(defaultPercentiles)
	}

	return &Histogram{
		interval:    interval,
		aggregates:  defaultAggregates,
		percentiles: defaultPercentiles,
	}
}

func (h *Histogram) configure(aggregates []string, percentiles []int) {
	h.aggregates = aggregates
	sort.Ints(percentiles)
	h.percentiles = percentiles
}

// neumaierAdd is one step of Neumaier's improved Kahan–Babuška compensated summation
// (s, c are running state, x the new term). See https://en.wikipedia.org/wiki/Kahan_summation_algorithm.
// Used when |s| vs |x| cannot be ordered a priori (mixed signs, cancellation possible).
func neumaierAdd(s, c, x float64) (float64, float64) {
	t := s + x
	if math.Abs(s) >= math.Abs(x) {
		c += (s - t) + x // s is bigger: low-order digits of x are lost, capture them.
	} else {
		c += (x - t) + s // x is bigger: low-order digits of s are lost, capture them.
	}
	return t, c
}

// sampleSum computes sum(value*weight) over h.samples using compensated summation.
//
// Precondition: flush() calls sort.Sort(h.samples) before invoking sampleSum, so samples
// are in ascending order of .value.
//
// Algorithm (for uniform weights, which is the dominant DogStatsD shape — one sample rate
// per metric): split the sorted slice into negatives [0, split) and non-negatives [split, n).
//
//   - Negatives are iterated in ascending order (most-negative first). Because every added
//     term is non-positive, |s| is monotone non-decreasing and |s| >= |x| holds for every
//     subsequent term, so the branch-free Fast2Sum step is exact.
//   - Non-negatives are iterated in reverse (largest first). By the same argument with
//     fl(a+b) >= max(a,b) for non-negative floats, |s| >= |x| holds and Fast2Sum is exact.
//   - The two partial sums are merged with a single Neumaier step so their accumulated
//     compensations combine correctly. This preserves bits that a naive final add would
//     drop when the two halves nearly cancel (e.g. {1, +1e100, 1, -1e100} = 2.0).
//
// For non-uniform weights, sort-by-value doesn't imply sort-by-term, so the |s|>=|x|
// invariant isn't guaranteed; fall back to full Neumaier over the whole slice.
//
// Both summation loops are written as inlined Fast2Sum steps (Dekker's primitive, the
// |s|>=|x| variant of the Kahan/Neumaier compensated-sum step):
//
//	t := s + x          // rounded sum
//	c += (s - t) + x    // exact rounding error captured into the compensation
//	s  = t
//
// Inlining matters: the 5-line body is the entire hot path, function-call overhead would
// dominate it, and keeping the running (s, c) in registers across iterations gives the
// out-of-order pipeline visible parallel work.
func (h *Histogram) sampleSum() float64 {
	n := len(h.samples)
	if n == 0 {
		return 0
	}

	if h.sharedWeight <= 0 {
		var s, c float64
		for _, ws := range h.samples {
			s, c = neumaierAdd(s, c, ws.value*float64(ws.weight))
		}
		return s + c
	}

	// Negative half, ascending. Walk from the left and stop at the first non-negative
	// element. Because samples are sorted, the break marks the split between halves, so
	// we find it and accumulate in a single pass (no separate scan — that would double
	// memory traffic and regresses large-N mixed-sign by ~5%).
	var sNeg, cNeg float64
	split := 0
	for split < n {
		ws := h.samples[split]
		if ws.value >= 0 {
			break
		}
		// Inlined Fast2Sum step (Kahan/Neumaier-family compensated-add, |sNeg|>=|x| branch).
		x := ws.value * float64(ws.weight)
		t := sNeg + x
		cNeg += (sNeg - t) + x
		sNeg = t
		split++
	}
	// Non-negative half, descending — s monotone non-decreasing, Fast2Sum is exact.
	var sPos, cPos float64
	for i := n - 1; i >= split; i-- {
		ws := h.samples[i]
		// Inlined Fast2Sum step (same compensated-add as above; precondition |sPos|>=|x|).
		x := ws.value * float64(ws.weight)
		t := sPos + x
		cPos += (sPos - t) + x
		sPos = t
	}
	// Merge the two partials with one Neumaier step (their magnitudes are unordered).
	// Passing c=0 makes the returned c the exact two-sum error of sNeg+sPos; fold in
	// cNeg and cPos so no compensation bits are dropped before the final s+c.
	s, c := neumaierAdd(sNeg, 0, sPos)
	return s + (c + cNeg + cPos)
}

//nolint:revive // TODO(AML) Fix revive linter
func (h *Histogram) addSample(sample *MetricSample, _ float64) {
	rate := sample.SampleRate
	if rate == 0 {
		rate = 1
	}

	w := int64(1 / rate)
	h.samples = append(h.samples, weightSample{sample.Value, w})
	h.count += w

	// Track whether all samples in this interval share the same weight; see sampleSum.
	switch {
	case h.sharedWeight == 0:
		h.sharedWeight = w
	case h.sharedWeight > 0 && h.sharedWeight != w:
		h.sharedWeight = -1
	}

	if h.unit == "" && sample.Unit != "" {
		h.unit = sample.Unit
	}
}

func (h *Histogram) flush(timestamp float64) ([]*Serie, error) {
	if len(h.samples) == 0 {
		return []*Serie{}, NoSerieError{}
	}

	sort.Sort(h.samples)

	series := make([]*Serie, 0, len(h.aggregates)+len(h.percentiles))

	// Compute the weighted sum only when a configured aggregate actually needs it.
	// This keeps flush cost to the sort+aggregate passes when only min/max/median/count are used.
	var sampleSum float64
	var sampleSumComputed bool
	ensureSampleSum := func() float64 {
		if !sampleSumComputed {
			sampleSum = h.sampleSum()
			sampleSumComputed = true
		}
		return sampleSum
	}

	// Compute aggregates
	for _, aggregate := range h.aggregates {
		var value float64
		mType := APIGaugeType
		switch aggregate {
		case maxAgg:
			value = h.samples[len(h.samples)-1].value
		case minAgg:
			value = h.samples[0].value
		case medianAgg:
			weight := int64(0)
			target := (h.count - 1) / 2
			for _, s := range h.samples {
				weight += s.weight
				if weight > target {
					value = s.value
					break
				}
			}
		case avgAgg:
			value = ensureSampleSum() / float64(h.count)
		case sumAgg:
			value = ensureSampleSum()
		case countAgg:
			value = float64(h.count) / float64(h.interval)
			mType = APIRateType
		default:
			log.Infof("Configured aggregate '%s' is not implemented, skipping", aggregate)
			continue
		}

		series = append(series, &Serie{
			Points:     []Point{{Ts: timestamp, Value: value}},
			MType:      mType,
			NameSuffix: "." + aggregate,
			Unit:       h.unit,
		})
	}

	// Compute percentiles
	target := make([]int64, 0, len(h.percentiles))
	for _, percentile := range h.percentiles {
		target = append(target, (int64(percentile)*h.count-1)/100)
	}

	if len(target) > 0 {
		weight := int64(0)
		idx := 0
		for _, s := range h.samples {
			weight += s.weight
			for idx < len(target) && weight > target[idx] {
				series = append(series, &Serie{
					Points:     []Point{{Ts: timestamp, Value: s.value}},
					MType:      APIGaugeType,
					NameSuffix: fmt.Sprintf(".%dpercentile", h.percentiles[idx]),
					Unit:       h.unit,
				})
				idx++
			}
			if idx >= len(h.percentiles) {
				break
			}
		}
	}

	// reset histogram
	h.samples = weightSamples{}
	h.count = 0
	h.unit = ""
	h.sharedWeight = 0

	return series, nil
}

func (h *Histogram) isStateful() bool {
	return false
}
