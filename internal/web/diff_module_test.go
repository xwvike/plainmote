package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"plainmote/internal/linediff"
)

// diff.js is the line diff for encrypted versions, which only the browser
// can read. It has to agree with internal/linediff line for line, or the
// same two versions would compare differently encrypted and not.
func TestDiffModuleMatchesGo(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	type testCase struct {
		Before string `json:"before"`
		After  string `json:"after"`
		Full   bool   `json:"full"`
	}
	cases := []testCase{
		{"", "", false}, {"a\n", "", false}, {"", "a\nb\n", false},
		{"a\nb\nc\n", "a\nB\nc\n", false}, {"a\r\nb\r\n", "a\nb\n", false},
		{"listen 80;\nserver_name a;\n", "listen 8080;\nserver_name a;\nroot /srv;\n", true},
		{strings.Repeat("x\n", 50) + "y\n" + strings.Repeat("x\n", 50), strings.Repeat("x\n", 50) + "z\n" + strings.Repeat("x\n", 50), false},
		{"no final newline", "no final newline\n", false},
		{strings.Repeat("a\n", 3000), strings.Repeat("b\n", 3000), false},
	}
	random := rand.New(rand.NewSource(7))
	words := []string{"a", "b", "c", "port: 1", "", "  x"}
	for i := 0; i < 200; i++ {
		var before, after strings.Builder
		for j := random.Intn(30); j > 0; j-- {
			fmt.Fprintln(&before, words[random.Intn(len(words))])
		}
		for j := random.Intn(30); j > 0; j-- {
			fmt.Fprintln(&after, words[random.Intn(len(words))])
		}
		cases = append(cases, testCase{before.String(), after.String(), i%3 == 0})
	}
	input, _ := json.Marshal(cases)
	script, err := os.ReadFile(filepath.Join("testdata", "diff_test.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	module, _ := filepath.Abs(filepath.Join("static", "diff.js"))
	command := exec.Command(node, "--input-type=module", "-e", string(script))
	command.Env = append(os.Environ(), "DIFF_MODULE=file://"+module)
	command.Stdin = bytes.NewReader(input)
	out, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		Lines []struct {
			Kind    string `json:"kind"`
			Old     int    `json:"old"`
			New     int    `json:"new"`
			Text    string `json:"text"`
			Skipped int    `json:"skipped"`
		} `json:"lines"`
		Added    int  `json:"added"`
		Removed  int  `json:"removed"`
		TooLarge bool `json:"tooLarge"`
	}
	if err := json.Unmarshal(out, &got); err != nil || len(got) != len(cases) {
		t.Fatalf("output: %v %s", err, out)
	}
	for i, c := range cases {
		want := linediff.Compare(c.Before, c.After, c.Full)
		js := got[i]
		if js.TooLarge != want.TooLarge || js.Added != want.Added || js.Removed != want.Removed || len(js.Lines) != len(want.Lines) {
			t.Fatalf("case %d: js %+v, go %+v", i, js, want)
		}
		for j, line := range want.Lines {
			l := js.Lines[j]
			if l.Kind != line.Kind || l.Old != line.Old || l.New != line.New || l.Text != line.Text || l.Skipped != line.Skipped {
				t.Fatalf("case %d line %d: js %+v, go %+v", i, j, l, line)
			}
		}
	}
}
