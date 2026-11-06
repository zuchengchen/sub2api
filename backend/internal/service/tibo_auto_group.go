package service

import (
	"context"
	"time"
)

// TiboAutoGroupSample is issued only by a newly completed active Tibo probe.
// StartedAt orders overlapping probes across processes; completion order must
// not let an older request undo a newer result.
type TiboAutoGroupSample struct {
	Verdict     string
	StartedAt   time.Time
	CompletedAt time.Time
}

// TiboAutoGroupRepository persists the observation and its bounded membership
// change atomically. Generic UpdateExtra and passive model observations cannot
// invoke this capability implicitly.
type TiboAutoGroupRepository interface {
	PersistTiboProbeResult(context.Context, int64, map[string]any, TiboAutoGroupSample) error
}
