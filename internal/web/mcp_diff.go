package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"plainmote/internal/store"
)

func (a *App) mcpDiffVersions(x mcpExchange, args json.RawMessage) (mcpOutcome, error) {
	var in struct {
		Resource string `json:"resource"`
		From     int    `json:"from"`
		To       int    `json:"to"`
	}
	if problem := mcpArguments(args, &in); problem != "" {
		return mcpProblem("%s", problem)
	}
	if in.From < 0 || in.To < 0 {
		return mcpProblem("Invalid arguments: from and to cannot be negative.")
	}
	resource, problem, err := a.mcpResolve(x, in.Resource)
	if err != nil || problem != "" {
		return mcpOutcome{problem: problem}, err
	}
	if mcpEncrypted(resource) {
		return mcpProblem("%s", mcpEncryptedText)
	}
	to, from := in.To, in.From
	if to == 0 {
		to = resource.Version
	}
	if from == 0 {
		earlier, err := a.db.ListVersions(x.r.Context(), x.user.ID, resource.ID)
		if err != nil {
			return mcpOutcome{}, err
		}
		for _, v := range earlier {
			if v.Number < to {
				from = v.Number
				break
			}
		}
		if from == 0 {
			return mcpProblem("%s has no version before %d to compare it with; list_versions shows the ones it has.", mcpLabel(resource), to)
		}
	}
	if from == to {
		return mcpProblem("Invalid arguments: from and to are both version %d.", to)
	}
	before, problem, err := a.mcpVersionText(x, resource, from)
	if err != nil || problem != "" {
		return mcpOutcome{problem: problem}, err
	}
	after, problem, err := a.mcpVersionText(x, resource, to)
	if err != nil || problem != "" {
		return mcpOutcome{problem: problem}, err
	}
	diff := lineDiff(before, after, false)
	structured := map[string]any{
		"id": resource.ID, "from": from, "to": to, "added": diff.Added, "removed": diff.Removed,
		"url": a.baseURL(x.r) + "/resources/" + resource.ID,
	}
	header := fmt.Sprintf("%s · version %d → %d", mcpLabel(resource), from, to)
	if diff.TooLarge {
		structured["too_large"] = true
		return mcpOutcome{structured: structured, text: []string{header + " · the versions differ too much to compare line by line; read each with read_resource."}}, nil
	}
	unified, truncated := unifiedDiff(diff, from, to)
	structured["diff"] = unified
	header += fmt.Sprintf(" · %d lines added, %d removed", diff.Added, diff.Removed)
	if truncated {
		structured["truncated"] = true
		header += fmt.Sprintf(" · the diff is cut at %d KiB", mcpReadChunk>>10)
	}
	return mcpOutcome{structured: structured, text: []string{header, unified}}, nil
}

// mcpVersionText is the whole text of a resource, now or at an earlier
// version. The problem says why there is none: no such version, or content
// that is encrypted or not text.
func (a *App) mcpVersionText(x mcpExchange, resource Resource, number int) (string, string, error) {
	ctx := x.r.Context()
	contentType, encoding := resource.ContentType, resource.ContentEncoding
	read := func() ([]byte, error) { return a.db.ReadContent(ctx, resource) }
	if number != resource.Version {
		earlier, err := a.db.VersionForOwner(ctx, x.user.ID, resource.ID, number)
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Sprintf("%s has no version %d; list_versions shows the ones it has.", mcpLabel(resource), number), nil
		}
		if err != nil {
			return "", "", err
		}
		if len(earlier.SealedMeta) > 0 || earlier.ContentType == store.SealedContentType || earlier.ContentType == store.EncryptedContentType {
			return "", mcpEncryptedText, nil
		}
		contentType, encoding = earlier.ContentType, earlier.ContentEncoding
		read = func() ([]byte, error) { return a.db.ReadVersion(ctx, earlier) }
	}
	notText := fmt.Sprintf("Version %d of %s is %s content, not text.", number, mcpLabel(resource), contentType)
	if !store.TextLike(contentType) {
		return "", notText, nil
	}
	stored, err := read()
	if err != nil {
		return "", "", err
	}
	text, _, err := store.DecodeText(stored, encoding)
	if err != nil {
		return "", notText, nil
	}
	return text, "", nil
}

// unifiedDiff writes a diff in the unified format, with three lines of
// context around each change, cut on a line once it reaches mcpReadChunk.
func unifiedDiff(diff textDiff, from, to int) (string, bool) {
	var out strings.Builder
	fmt.Fprintf(&out, "--- version %d\n+++ version %d\n", from, to)
	// The line each side is at: the last one written or folded away.
	oldAt, newAt := 0, 0
	lines := diff.Lines
	for len(lines) > 0 {
		if lines[0].Kind == "fold" {
			oldAt += lines[0].Skipped
			newAt += lines[0].Skipped
			lines = lines[1:]
			continue
		}
		end := 0
		oldCount, newCount := 0, 0
		for end < len(lines) && lines[end].Kind != "fold" {
			switch lines[end].Kind {
			case "=":
				oldCount++
				newCount++
			case "-":
				oldCount++
			case "+":
				newCount++
			}
			end++
		}
		var hunk strings.Builder
		fmt.Fprintf(&hunk, "@@ -%s +%s @@\n", hunkRange(oldAt, oldCount), hunkRange(newAt, newCount))
		for _, line := range lines[:end] {
			mark := " "
			if line.Kind != "=" {
				mark = line.Kind
			}
			hunk.WriteString(mark + line.Text + "\n")
		}
		for _, row := range strings.SplitAfter(hunk.String(), "\n") {
			if out.Len()+len(row) > mcpReadChunk {
				return out.String(), true
			}
			out.WriteString(row)
		}
		oldAt += oldCount
		newAt += newCount
		lines = lines[end:]
	}
	return out.String(), false
}

// hunkRange is one side of a hunk header: where it starts and how many lines
// it spans. A side with no lines names the line before it, as diff does.
func hunkRange(at, count int) string {
	start := at + 1
	if count == 0 {
		start = at
	}
	if count == 1 {
		return fmt.Sprint(start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}
