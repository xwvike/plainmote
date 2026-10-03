package web

import "plainmote/internal/linediff"

// The version pages' diff lives in its own package, shared with the command
// line; these names keep the handlers and templates reading as they did.
type (
	diffLine = linediff.Line
	textDiff = linediff.Diff
)

const diffMaxEdits = linediff.MaxEdits

func lineDiff(before, after string, full bool) textDiff {
	return linediff.Compare(before, after, full)
}
