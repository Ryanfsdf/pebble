// Copyright 2019 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/petermattis/pebble/internal/rate"
)

var nilPacer = &noopPacer{}

type limiter interface {
	WaitN(ctx context.Context, n int) (err error)
	AllowN(now time.Time, n int) bool
	Burst() int
}

// pacer is the interface for flush and compaction rate limiters. The rate limiter
// is possible applied on each iteration step of a flush or compaction. This is to
// limit background IO usage so that it does not contend with foreground traffic.
type pacer interface {
	maybeThrottle(bytesIterated uint64) error
}

// internalPacer contains fields and methods common to both compactionPacer and
// flushPacer.
type internalPacer struct {
	limiter limiter

	iterCount             uint64
	prevBytesIterated     uint64
	refreshBytesThreshold uint64
	slowdownThreshold     uint64
}

// limit applies rate limiting if the current byte level is below the configured
// threshold.
func (p *internalPacer) limit(amount, currentLevel uint64) error {
	if currentLevel <= p.slowdownThreshold {
		burst := p.limiter.Burst()
		for amount > uint64(burst) {
			err := p.limiter.WaitN(context.Background(), burst)
			if err != nil {
				return err
			}
			amount -= uint64(burst)
		}
		err := p.limiter.WaitN(context.Background(), int(amount))
		if err != nil {
			return err
		}
	} else {
		burst := p.limiter.Burst()
		for amount > uint64(burst) {
			p.limiter.AllowN(time.Now(), burst)
			amount -= uint64(burst)
		}
		p.limiter.AllowN(time.Now(), int(amount))
	}
	return nil
}

// compactionPacerInfo contains information necessary for compaction pacing.
type compactionPacerInfo struct {
	// slowdownThreshold is the low watermark for compaction debt. If compaction debt is
	// below this threshold, we slow down compactions. If compaction debt is above this
	// threshold, we let compactions continue as fast as possible. We want to keep
	// compaction speed as slow as possible to match the speed of flushes. This threshold
	// is set so that a single flush cannot contribute enough compaction debt to overshoot
	// the threshold.
	slowdownThreshold   uint64
	totalCompactionDebt uint64
}

// compactionPacerEnv defines the environment in which the compaction rate limiter
// is applied.
type compactionPacerEnv struct {
	limiter      limiter
	memTableSize uint64

	getInfo func() compactionPacerInfo
}

// compactionPacer rate limits compactions depending on compaction debt. The rate
// limiter is applied at a rate that keeps compaction debt at a steady level. If
// compaction debt increases at a rate that is faster than the system can handle,
// no rate limit is applied.
type compactionPacer struct {
	internalPacer
	env                 compactionPacerEnv
	totalCompactionDebt uint64
}

func newCompactionPacer(env compactionPacerEnv) *compactionPacer {
	return &compactionPacer{
		env: env,
		internalPacer: internalPacer{
			limiter: env.limiter,
		},
	}
}

// maybeThrottle slows down compactions to match memtable flush rate. The DB
// provides a compaction debt estimate and a slowdown threshold. We subtract the
// compaction debt estimate by the bytes iterated in the current compaction. If
// the new compaction debt estimate is below the threshold, the rate limiter is
// applied. If the new compaction debt is above the threshold, the rate limiter
// is not applied.
func (p *compactionPacer) maybeThrottle(bytesIterated uint64) error {
	if bytesIterated == 0 {
		return errors.New("pebble: maybeThrottle supplied with invalid bytesIterated")
	}

	// Recalculate total compaction debt and the slowdown threshold only once
	// every 1000 iterations or when the refresh threshold is hit since it
	// requires grabbing DB.mu which is expensive.
	if p.iterCount == 0 || bytesIterated > p.refreshBytesThreshold {
		pacerInfo := p.env.getInfo()
		p.slowdownThreshold = pacerInfo.slowdownThreshold
		p.totalCompactionDebt = pacerInfo.totalCompactionDebt
		p.refreshBytesThreshold = bytesIterated + (p.env.memTableSize*5/100)
		p.iterCount = 1000
	}
	p.iterCount--

	var curCompactionDebt uint64
	if p.totalCompactionDebt > bytesIterated {
		curCompactionDebt = p.totalCompactionDebt - bytesIterated
	}

	compactAmount := bytesIterated - p.prevBytesIterated
	p.prevBytesIterated = bytesIterated

	// We slow down compactions when the compaction debt falls below the slowdown
	// threshold, which is set dynamically based on the number of non-empty levels.
	// This will only occur if compactions can keep up with the pace of flushes. If
	// bytes are flushed faster than how fast compactions can occur, compactions
	// proceed at maximum (unthrottled) speed.
	return p.limit(compactAmount, curCompactionDebt)
}

// flushPacerInfo contains information necessary for compaction pacing.
type flushPacerInfo struct {
	totalBytes uint64
}

// flushPacerEnv defines the environment in which the compaction rate limiter is
// applied.
type flushPacerEnv struct {
	limiter      limiter
	memTableSize uint64

	getInfo func() flushPacerInfo
}

// flushPacer rate limits memtable flushing to match the speed of incoming user
// writes. If user writes come in faster than the memtable can be flushed, no
// rate limit is applied.
type flushPacer struct {
	internalPacer
	env        flushPacerEnv
	totalBytes uint64
}

func newFlushPacer(env flushPacerEnv) *flushPacer {
	return &flushPacer{
		env: env,
		internalPacer: internalPacer{
			limiter:           env.limiter,
			slowdownThreshold: env.memTableSize*105/100,
		},
	}
}

// maybeThrottle slows down memtable flushing to match user write rate. The DB
// provides the total number of bytes in all the memtables. We subtract this total
// by the number of bytes flushed in the current flush to get a "dirty byte" count.
// If the dirty byte count is below the watermark (105% memtable size), the rate
// limiter is applied. If the dirty byte count is above the watermark, the rate
// limiter is not applied.
func (p *flushPacer) maybeThrottle(bytesIterated uint64) error {
	if bytesIterated == 0 {
		return errors.New("pebble: maybeThrottle supplied with invalid bytesIterated")
	}

	// Recalculate total memtable bytes only once every 1000 iterations or
	// when the refresh threshold is hit since getting the total memtable
	// byte count requires grabbing DB.mu which is expensive.
	if p.iterCount == 0 || bytesIterated > p.refreshBytesThreshold {
		pacerInfo := p.env.getInfo()
		p.totalBytes = pacerInfo.totalBytes
		p.refreshBytesThreshold = bytesIterated + (p.env.memTableSize*5/100)
		p.iterCount = 1000
	}
	p.iterCount--

	// dirtyBytes is the total number of bytes in the memtables minus the number of
	// bytes flushed. It represents unflushed bytes in all the memtables, even the
	// ones which aren't being flushed such as the mutable memtable.
	dirtyBytes := p.totalBytes - bytesIterated
	flushAmount := bytesIterated - p.prevBytesIterated
	p.prevBytesIterated = bytesIterated

	// We slow down memtable flushing when the dirty bytes indicator falls
	// below the low watermark, which is 105% memtable size. This will only
	// occur if memtable flushing can keep up with the pace of incoming
	// writes. If writes come in faster than how fast the memtable can flush,
	// flushing proceeds at maximum (unthrottled) speed.
	return p.limit(flushAmount, dirtyBytes)
}

const (
	// Interval in which we readjust the rate (in ms).
	recalculateInterval = time.Millisecond * 100
	refillsPerSecond    = 10

	maximumRate          = 1000 << 20 // 1 GB/s
	minimumRate          = 50 << 20 // 50 MB/s
	lowWatermarkPercent  = 50
	highWatermarkPercent = 90

	// Tune by 5% each time.
	adjustmentFactorPercent = 5
)

type autoTunedCompactionPacer struct {
	limiter *rate.Limiter

	// last time the limiter was adjusted
	lastRefresh time.Time

	curAmount   int
	maxCapacity int

	prevBytesIterated uint64
}

func newAutoTunedCompactionPacer() *autoTunedCompactionPacer {
	return &autoTunedCompactionPacer{
		limiter:     rate.NewLimiter(maximumRate, math.MaxInt32),
		lastRefresh: time.Now(),
		maxCapacity: maximumRate / refillsPerSecond,
	}
}

func (p *autoTunedCompactionPacer) maybeThrottle(bytesIterated uint64) error {
	var compactAmount int
	if bytesIterated > p.prevBytesIterated {
		compactAmount = int(bytesIterated - p.prevBytesIterated)
	}
	p.prevBytesIterated = bytesIterated

	err := p.limiter.WaitN(context.Background(), int(compactAmount))
	if err != nil {
		return err
	}

	p.curAmount += compactAmount

	now := time.Now()
	elapsedTime := now.Sub(p.lastRefresh)
	if elapsedTime > recalculateInterval {
		p.lastRefresh = now

		drainedPercent := uint64(p.curAmount / p.maxCapacity) *
			uint64(elapsedTime / recalculateInterval) * 100

		if drainedPercent < lowWatermarkPercent {
			limit := uint64(p.limiter.Limit()) * (100 - adjustmentFactorPercent) / 100
			if uint64(limit) > minimumRate {
				p.limiter.SetLimit(rate.Limit(limit))
			}
		} else if drainedPercent > highWatermarkPercent {
			limit := uint64(p.limiter.Limit()) * (100 + adjustmentFactorPercent) / 100
			if uint64(limit) < maximumRate {
				p.limiter.SetLimit(rate.Limit(limit))
			}
		}

		p.curAmount = 0
		p.maxCapacity = int(p.limiter.Limit()) / refillsPerSecond
	}

	return nil
}

type noopPacer struct {}

func (p *noopPacer) maybeThrottle(_ uint64) error {
	return nil
}
