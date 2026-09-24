package web

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// probeWindow is how often folded probe counts reach stderr. Short enough to
// show a burst while it is still happening, long enough that the burst cannot
// turn stderr into the flood it is reporting.
const probeWindow = time.Minute

const (
	probeMalformed = "malformed"
	probeUnknown   = "unknown"
	// probeEmbed is another site's page loading a share address as an image,
	// video, frame or fetch. It is refused before the token is looked at.
	probeEmbed = "embed"
)

// probeReasons fixes the order of the folded line so two reports compare by eye.
var probeReasons = []string{probeMalformed, probeUnknown, probeEmbed}

// probeLog folds delivery probes that belong to nobody into at most one line
// per window. These events cannot go in access_logs: every row there is read
// through the owning resource, so a row with no resource is one no user could
// ever see, and an anonymous caller could write them without limit. Dropping
// them silently is worse - a service blind to token guessing cannot tell an
// attack from a quiet day - so the count is kept and reported instead.
//
// The zero value is ready to use, because the handlers are also built as bare
// struct literals in tests.
type probeLog struct {
	mu     sync.Mutex
	counts map[string]int64
	since  time.Time
	next   time.Time
}

// record counts one probe and returns the line to report, if the window closed.
// Nothing is lost while it stays open: the count carries into the next line.
func (p *probeLog) record(reason string, now time.Time) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.counts == nil {
		p.counts = map[string]int64{}
		p.since, p.next = now, now.Add(probeWindow)
	}
	p.counts[reason]++
	if now.Before(p.next) {
		return ""
	}
	line := fmt.Sprintf("delivery probes since %s:", p.since.Format(time.RFC3339))
	for _, name := range probeReasons {
		if count := p.counts[name]; count > 0 {
			line += fmt.Sprintf(" %s=%d", name, count)
		}
	}
	clear(p.counts)
	p.since, p.next = now, now.Add(probeWindow)
	return line
}

func (a *App) recordProbe(reason string) {
	if line := a.probes.record(reason, time.Now().UTC()); line != "" {
		fmt.Fprintln(os.Stderr, line)
	}
}
