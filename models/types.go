package models

import "time"

// Comparison is the result of comparing an original vs migrated response
// for a single mirrored request.
type Comparison struct {
	ID                  int64
	RunID               string
	Method              string
	Path                string
	Query               string

	StatusOriginal int
	StatusMigrated int

	StatusMatch bool
	HeaderMatch bool

	// HeaderMismatches lists request/response header names that differed.
	HeaderMismatches []string

	BodyMatch bool

	// BodyPreviewOriginal / BodyPreviewMigrated are truncated response bodies
	// stored for debugging (respects report.store_bodies config).
	BodyPreviewOriginal string
	BodyPreviewMigrated string

	// BodyOriginalFull / BodyMigratedFull hold the complete response bodies
	// for records that had a discrepancy (respects report.store_full_bodies).
	BodyOriginalFull string
	BodyMigratedFull string

	// DiffPaths lists the JSON paths where the two bodies diverged
	// (e.g. "data.items[0].price").
	DiffPaths []string

	// Performance metrics.
	DurationOriginal time.Duration
	DurationMigrated time.Duration
	SizeOriginal     int
	SizeMigrated     int

	// ErrorOriginal / ErrorMigrated capture transport-level failures.
	ErrorOriginal string
	ErrorMigrated string

	// IsMatch is true only when status, headers, and body all match and both
	// services responded without transport errors.
	IsMatch bool

	CreatedAt time.Time
}

// LatencySummary holds computed latency statistics for a group of requests.
type LatencySummary struct {
	Count     int
	Avg       time.Duration
	Median    time.Duration
	P95       time.Duration
	Max       time.Duration
}