// Package linediff compares two texts line by line, for the version pages and
// for the command line's conflict report.
package linediff

import "strings"

// Line is one row of a unified diff: a line both sides share ("="), one
// only the old side has ("-"), one only the new side has ("+"), or a run of
// shared lines folded away ("fold", with Skipped saying how many). Line
// numbers are 1-based and 0 where a side has no line.
type Line struct {
	Kind    string
	Old     int
	New     int
	Text    string
	Skipped int
}

type Diff struct {
	Lines   []Line
	Added   int
	Removed int
	// TooLarge is a diff given up on: too many lines or too many changes to
	// work out and show within reason. Nothing else is set.
	TooLarge bool
}

const (
	Context = 3
	// Bounds on the work a page view may cause. Configs and logs are far below
	// both; a rewritten 10 MiB file is not, and gets a sentence instead.
	MaxLines = 200_000
	MaxEdits = 2_000
)

// splitLines cuts text into lines for comparison. A final newline ends the
// last line rather than starting an empty one, and a carriage return before
// a newline is not part of the line: a file whose line endings changed is
// otherwise one where every line did.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	return lines
}

// Compare compares two texts line by line. Shared runs longer than the
// context around a change are folded unless full is set.
func Compare(before, after string, full bool) Diff {
	a, b := splitLines(before), splitLines(after)
	if len(a)+len(b) > MaxLines {
		return Diff{TooLarge: true}
	}
	ops, ok := diffOps(a, b)
	if !ok {
		return Diff{TooLarge: true}
	}

	var rows []Line
	var result Diff
	oldLine, newLine := 1, 1
	for _, op := range ops {
		switch op {
		case '=':
			rows = append(rows, Line{Kind: "=", Old: oldLine, New: newLine, Text: a[oldLine-1]})
			oldLine++
			newLine++
		case '-':
			rows = append(rows, Line{Kind: "-", Old: oldLine, Text: a[oldLine-1]})
			oldLine++
			result.Removed++
		case '+':
			rows = append(rows, Line{Kind: "+", New: newLine, Text: b[newLine-1]})
			newLine++
			result.Added++
		}
	}
	if full {
		result.Lines = rows
		return result
	}

	keep := make([]bool, len(rows))
	for i, row := range rows {
		if row.Kind == "=" {
			continue
		}
		for j := max(i-Context, 0); j <= min(i+Context, len(rows)-1); j++ {
			keep[j] = true
		}
	}
	skipped := 0
	for i, row := range rows {
		if keep[i] {
			if skipped > 0 {
				result.Lines = append(result.Lines, Line{Kind: "fold", Skipped: skipped})
				skipped = 0
			}
			result.Lines = append(result.Lines, row)
			continue
		}
		skipped++
	}
	if skipped > 0 {
		result.Lines = append(result.Lines, Line{Kind: "fold", Skipped: skipped})
	}
	return result
}

// diffOps is Myers' shortest edit script between a and b, as a sequence of
// '=', '-' and '+'. The common head and tail are taken off first, which is
// most of any real config change, and the search stops at MaxEdits.
func diffOps(a, b []string) ([]byte, bool) {
	head := 0
	for head < len(a) && head < len(b) && a[head] == b[head] {
		head++
	}
	tail := 0
	for tail < len(a)-head && tail < len(b)-head && a[len(a)-1-tail] == b[len(b)-1-tail] {
		tail++
	}
	middle, ok := myers(internLines(a[head:len(a)-tail], b[head:len(b)-tail]))
	if !ok {
		return nil, false
	}
	ops := make([]byte, 0, head+len(middle)+tail)
	for range head {
		ops = append(ops, '=')
	}
	ops = append(ops, middle...)
	for range tail {
		ops = append(ops, '=')
	}
	return ops, true
}

// internLines turns lines into numbers, so the search compares integers.
func internLines(a, b []string) ([]int, []int) {
	ids := make(map[string]int, len(a)+len(b))
	number := func(lines []string) []int {
		out := make([]int, len(lines))
		for i, line := range lines {
			id, ok := ids[line]
			if !ok {
				id = len(ids)
				ids[line] = id
			}
			out[i] = id
		}
		return out
	}
	return number(a), number(b)
}

func myers(a, b []int) ([]byte, bool) {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil, true
	}
	limit := min(n+m, MaxEdits)
	offset := limit + 1
	v := make([]int, 2*limit+3)
	// trace[d] is v as it stood before step d, over the diagonals step d
	// reads: -d-1 to d+1. Kept that narrow, the whole trace is about d².
	var trace [][]int
	for d := 0; d <= limit; d++ {
		trace = append(trace, append([]int(nil), v[offset-d-1:offset+d+2]...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				return backtrack(trace, n, m), true
			}
		}
	}
	return nil, false
}

func backtrack(trace [][]int, n, m int) []byte {
	var ops []byte
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		v := trace[d]
		at := func(k int) int { return v[k+d+1] }
		k := x - y
		var prevK int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := at(prevK)
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			ops = append(ops, '=')
			x--
			y--
		}
		if d > 0 {
			if x == prevX {
				ops = append(ops, '+')
			} else {
				ops = append(ops, '-')
			}
		}
		x, y = prevX, prevY
	}
	// Built from the end; within a change, removals read better first.
	for i, j := 0, len(ops)-1; i < j; i, j = i+1, j-1 {
		ops[i], ops[j] = ops[j], ops[i]
	}
	return removalsFirst(ops)
}

// removalsFirst reorders each run of changes so its removals come before its
// additions, the way a reader expects a replaced line to read.
func removalsFirst(ops []byte) []byte {
	for start := 0; start < len(ops); {
		if ops[start] == '=' {
			start++
			continue
		}
		end := start
		removed := 0
		for end < len(ops) && ops[end] != '=' {
			if ops[end] == '-' {
				removed++
			}
			end++
		}
		for i := start; i < end; i++ {
			if i-start < removed {
				ops[i] = '-'
			} else {
				ops[i] = '+'
			}
		}
		start = end
	}
	return ops
}
