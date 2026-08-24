// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package eval

import "fmt"

// aboutWidth caps the one-line description. The text report puts About in the
// last column of a table that has to fit an 80-column terminal, and this is
// what is left after the widest label, value and range.
const aboutWidth = 60

// MetricInfo describes one metric so every renderer says the same thing about
// it. It says what the metric measures and where it sits, never whether the
// number is good: that reading belongs to whoever is looking at the feed.
//
// Best is the value the metric is best at, which is where the direction comes
// from: equal to Max means higher is better, equal to Min means lower is, and
// anything between means aim for it.
type MetricInfo struct {
	Key   string
	Label string
	Min   float64
	Max   float64
	Best  float64
	About string
}

// Range renders the bounds and the direction as one cell, e.g. "-1..1 up".
func (m MetricInfo) Range() string {
	var direction string
	switch m.Best {
	case m.Max:
		direction = "up"
	case m.Min:
		direction = "down"
	default:
		direction = fmt.Sprintf("->%s", trimFloat(m.Best))
	}
	return fmt.Sprintf("%s..%s %s", trimFloat(m.Min), trimFloat(m.Max), direction)
}

// metricCatalog is every metric the reports name, in the order they are shown.
type metricCatalog []MetricInfo

// byKey finds a definition, and is deliberately fatal-free: a renderer asking
// for a metric with no entry gets a zero value, and the catalog test is what
// keeps that from happening.
func (c metricCatalog) byKey(key string) MetricInfo {
	for _, m := range c {
		if m.Key == key {
			return m
		}
	}
	return MetricInfo{Key: key, Label: key}
}

// scoringMetrics describe how close the model landed to the golden scores.
var scoringMetrics = metricCatalog{
	{
		Key: "qwk", Label: "QWK",
		Min: -1, Max: 1, Best: 1,
		About: "agreement beyond what chance alone gives",
	},
	{
		Key: "mae", Label: "MAE",
		Min: MinScore, Max: MaxScore, Best: MinScore,
		About: "mean distance from the golden score",
	},
	{
		Key: "rmse", Label: "RMSE",
		Min: MinScore, Max: MaxScore, Best: MinScore,
		About: "the same, weighting big misses more",
	},
	{
		Key: "signed_mean", Label: "SIGNED MEAN",
		Min: -MaxScore, Max: MaxScore, Best: 0,
		About: "which way the model leans overall",
	},
}

// highSignalMetrics describe the decision the feed page actually makes.
var highSignalMetrics = metricCatalog{
	{
		Key: "high_signal.precision", Label: "PRECISION",
		Min: 0, Max: 1, Best: 1,
		About: "of what it badges, how much you want",
	},
	{
		Key: "high_signal.recall", Label: "RECALL",
		Min: 0, Max: 1, Best: 1,
		About: "of what you want, how much it badges",
	},
}

// trimFloat prints a bound without a trailing ".0", so a 0-10 range reads
// "0..10" rather than "0.0..10.0".
func trimFloat(v float64) string {
	if v == float64(int(v)) {
		return fmt.Sprintf("%d", int(v))
	}
	return fmt.Sprintf("%g", v)
}
