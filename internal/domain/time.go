package domain

import "time"

// EpochMS is wall-clock milliseconds since the Unix epoch. Every persisted *At
// timestamp is an EpochMS, EXCEPT the
// debug-log header/line timestamps (ISO-8601) and the log-filename date.
type EpochMS = int64

// NowMS returns the current time as epoch milliseconds.
func NowMS() EpochMS {
	return time.Now().UnixMilli()
}

// ToTime converts an epoch-ms value back to a time.Time (local clock).
func ToTime(ms EpochMS) time.Time {
	return time.UnixMilli(ms)
}

// FromTime converts a time.Time to epoch milliseconds.
func FromTime(t time.Time) EpochMS {
	return t.UnixMilli()
}
