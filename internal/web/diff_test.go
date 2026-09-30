package web

import (
	"math/rand"
	"strings"
	"testing"
)

// apply rebuilds both sides from the rows of an unfolded diff.
func apply(rows []diffLine) (before, after []string) {
	for _, row := range rows {
		switch row.Kind {
		case "=":
			before, after = append(before, row.Text), append(after, row.Text)
		case "-":
			before = append(before, row.Text)
		case "+":
			after = append(after, row.Text)
		}
	}
	return before, after
}

func TestLineDiffRebuildsBothSides(t *testing.T) {
	random := rand.New(rand.NewSource(8964))
	words := []string{"a", "b", "c", "d", "e"}
	for round := range 500 {
		var a, b []string
		for range random.Intn(30) {
			a = append(a, words[random.Intn(len(words))])
		}
		for range random.Intn(30) {
			b = append(b, words[random.Intn(len(words))])
		}
		before, after := strings.Join(a, "\n"), strings.Join(b, "\n")
		diff := lineDiff(before, after, true)
		gotA, gotB := apply(diff.Lines)
		if strings.Join(gotA, "\n") != before || strings.Join(gotB, "\n") != after {
			t.Fatalf("round %d: diff of %q -> %q does not rebuild them", round, before, after)
		}
		if diff.Added != len(b)-len(gotA)+diff.Removed {
			t.Fatalf("round %d: counts disagree: +%d -%d", round, diff.Added, diff.Removed)
		}
	}
}

func TestLineDiffIsMinimalAndReadable(t *testing.T) {
	before := "server:\n  port: 80\n  host: a\nlog: info\n"
	after := "server:\n  port: 8080\n  host: a\nlog: info\nextra: 1\n"
	diff := lineDiff(before, after, true)
	if diff.Added != 2 || diff.Removed != 1 {
		t.Fatalf("one line changed and one added: +%d -%d", diff.Added, diff.Removed)
	}
	// The replaced line reads as removed, then added, with numbers on both sides.
	if diff.Lines[1].Kind != "-" || diff.Lines[1].Old != 2 || diff.Lines[2].Kind != "+" || diff.Lines[2].New != 2 {
		t.Fatalf("unexpected rows: %+v", diff.Lines)
	}
}

func TestLineDiffFoldsSharedRuns(t *testing.T) {
	var lines []string
	for i := range 40 {
		lines = append(lines, "line "+strings.Repeat("x", i))
	}
	before := strings.Join(lines, "\n")
	lines[20] = "changed"
	after := strings.Join(lines, "\n")

	diff := lineDiff(before, after, false)
	if len(diff.Lines) != 1+3+2+3+1 {
		t.Fatalf("a fold, three lines of context, the change, three more and a fold: got %d rows", len(diff.Lines))
	}
	if diff.Lines[0].Kind != "fold" || diff.Lines[0].Skipped != 17 || diff.Lines[len(diff.Lines)-1].Skipped != 16 {
		t.Fatalf("folds should count what they hide: %+v", diff.Lines)
	}
}

func TestLineDiffIgnoresLineEndingsAndGivesUpOnHugeChanges(t *testing.T) {
	if diff := lineDiff("a\r\nb\r\n", "a\nb\n", false); diff.Added+diff.Removed != 0 {
		t.Fatalf("CRLF against LF is the same text: %+v", diff)
	}
	var a, b []string
	for i := range diffMaxEdits + 10 {
		a = append(a, "a"+strings.Repeat("-", i))
		b = append(b, "b"+strings.Repeat("-", i))
	}
	if diff := lineDiff(strings.Join(a, "\n"), strings.Join(b, "\n"), false); !diff.TooLarge {
		t.Fatalf("a rewrite past the edit bound should be given up on")
	}
}
