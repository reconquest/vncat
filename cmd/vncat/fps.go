package main

import "time"

type frameRate struct {
	since  time.Time
	frames uint64
	value  float64
}

func (rate *frameRate) sample(now time.Time) {
	elapsed := now.Sub(rate.since)
	if elapsed < time.Second {
		return
	}

	rate.value = float64(rate.frames) / elapsed.Seconds()
	rate.frames = 0
	rate.since = now
}
