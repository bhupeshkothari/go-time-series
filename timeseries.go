package timeseries

import (
	"errors"
	"time"
)

// Explanation
// Have several granularity buckets
// 1s, 1m, 5m, ...
// The buckets will be in circular arrays
//
// For example we could have
// 60 1s buckets to make up 1 minute
// 60 1m buckets to make up 1 hour
// ...
// This would enable us to get the last 1 minute data at 1s granularity (every second)
//
// Date ranges are [start, end[
//
// Put:
// Every time an event comes we add it to all corresponding buckets
//
// Example:
// Event time = 12:00:00
// 1s bucket = 12:00:00
// 1m bucket = 12:00:00
// 5m bucket = 12:00:00
//
// Event time = 12:00:01
// 1s bucket = 12:00:01
// 1m bucket = 12:00:00
// 5m bucket = 12:00:00
//
// Event time = 12:01:01
// 1s bucket = 12:01:01
// 1m bucket = 12:01:00
// 5m bucket = 12:00:00
//
// Fetch:
// Given a time span we try to find the buckets with the finest granularity
// to satisfy the time span and return the sum of their contents
//
// Example:
// Now = 12:05:30
// Time span = 12:05:00 - 12:05:02
// Return sum of 1s buckets 0,1
//
// Now = 12:10:00
// Time span = 12:05:00 - 12:07:00
// Return sum of 1m buckets 5,6
//
// Now = 12:10:00
// Time span = 12:00:00 - 12:10:00 (last 10 minutes)
// Return sum of 5m buckets 0,1
//
// Now = 12:10:01
// Time span = 12:05:01 - 12:10:01 (last 5 minutes)
// Return sum of 5m buckets (59/(5*60))*1, (1/(5*60))*2
//
// Now = 12:10:01
// Time span = 12:04:01 - 12:10:01 (last 6 minutes)
// Return sum of 1m buckets (59/60)*4, 5, 6, 7, 8, 9, (1/60)*10

var (
	ErrBadRange         = errors.New("range is invalid")
	ErrBadGranularities = errors.New("granularities must be strictly increasing and non empty")
	ErrRangeNotCovered  = errors.New("range is not convered")
)

var defaultGranularities = []time.Duration{
	time.Second,
	time.Minute,
	time.Hour,
}

type Clock interface {
	Now() time.Time
}

type defaultClock struct{}

func (c *defaultClock) Now() time.Time {
	return time.Now()
}

type TimeSeries interface {
	Increase(amount int)
	IncreaseAtTime(amount int, insertTime time.Time)
	Recent(duration time.Duration) (float64, error)
	Range(start, end time.Time) (float64, error)
}

type timeseries struct {
	clock       Clock
	levels      []level
	pending     int
	pendingTime time.Time
}

// NewTimeseries creates a new TimeSeries with default granularities
func NewTimeseries() TimeSeries {
	return NewTimeseriesWithGranularities(defaultGranularities)
}

// NewTimeseriesWithGranularities creates a new TimeSeries with provided granularities
// ErrBadGranularities is returned if granularities are not in increasing order.
func NewTimeseriesWithGranularities(granularities []time.Duration) TimeSeries {
	err := checkGranularities(granularities)
	if err != nil {
		panic(err)
	}
	clock := &defaultClock{}
	return &timeseries{clock: clock, levels: createLevels(clock, granularities)}
}

func checkGranularities(granularities []time.Duration) error {
	if len(granularities) == 0 {
		return ErrBadGranularities
	}
	last := granularities[0]
	for i := 1; i < len(granularities); i++ {
		if granularities[i] <= last {
			return ErrBadGranularities
		}
		last = granularities[i]
	}
	return nil
}

func createLevels(clock Clock, granularities []time.Duration) []level {
	levels := make([]level, len(granularities))
	for i := range granularities {
		levels[i] = newLevel(clock, granularities[i], 60)
	}
	return levels
}

// Increase adds amount at current time
func (t *timeseries) Increase(amount int) {
	t.IncreaseAtTime(amount, t.clock.Now())
}

// IncreaseAtTime adds amount at a specific time
func (t *timeseries) IncreaseAtTime(amount int, time time.Time) {
	if time.After(t.pendingTime) {
		t.advance(time)
		t.handlePending()
		t.pendingTime = t.levels[0].latest()
		t.pending = amount
	} else if time.After(t.pendingTime.Add(-t.levels[0].granularity)) {
		t.pending++
	} else {
		t.increaseAtTime(amount, time)
	}
}

func (t *timeseries) increaseAtTime(amount int, time time.Time) {
	for i := range t.levels {
		if time.Before(t.levels[i].latest().Add(-1 * t.levels[i].duration())) {
			continue
		}
		t.levels[i].increaseAtTime(amount, time)
	}
}

func (t *timeseries) handlePending() {
	t.increaseAtTime(t.pending, t.pendingTime)
	t.pending = 0
}

func (t *timeseries) advance(target time.Time) {
	for i := range t.levels {
		if !target.Before(t.levels[i].latest().Add(t.levels[i].duration())) {
			t.levels[i].clear(target)
			continue
		}
		t.levels[i].advance(target)
	}
}

// Recent returns the sum over [now-duration, now)
func (t *timeseries) Recent(duration time.Duration) (float64, error) {
	// TODO: advance to now
	now := t.clock.Now()
	return t.Range(now.Add(-duration), now)
}

// Range returns the sum over the given range [start, end)
// ErrBadRange is returned if start is after end.
// ErrRangeNotCovered is returned if the range lies outside the time series.
func (t *timeseries) Range(start, end time.Time) (float64, error) {
	if start.After(end) {
		return 0, ErrBadRange
	}
	if ok, err := t.intersects(start, end); !ok {
		return 0, err
	}
	//start, end = t.clamp(start, end)
	for i := range t.levels {
		// use !start.Before so earliest() is included
		// if we use earliest().Before() we won't get start
		if !start.Before(t.levels[i].earliest()) {
			return t.levels[i].sumInterval(start, end), nil
		}
	}
	return 0, nil
}

func (t *timeseries) intersects(start, end time.Time) (bool, error) {
	biggestLevel := t.levels[len(t.levels)-1]
	if end.Before(biggestLevel.latest().Add(-biggestLevel.duration())) {
		return false, ErrRangeNotCovered
	}
	if start.After(t.levels[0].latest()) {
		return false, ErrRangeNotCovered
	}
	return true, nil
}