package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runReviewState runs scripts/review-state.sh on a saved GraphQL response
// (testdata/review-state/<fixture>.json) and returns its output. A `gh` that
// fails the test sits first on PATH, so --from proves it never calls GitHub.
func runReviewState(t *testing.T, fixture string, args ...string) string {
	t.Helper()
	bin := t.TempDir()
	marker := filepath.Join(bin, "gh-called")
	fake := "#!/bin/sh\ntouch '" + marker + "'\nexit 99\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fake), 0o755); err != nil { //nolint:gosec // G306: the fake gh must be executable
		t.Fatal(err)
	}
	argv := append([]string{"900", "--from", filepath.Join("testdata", "review-state", fixture+".json")}, args...)
	cmd := exec.Command("./review-state.sh", argv...)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("review-state.sh %s: %v\n%s", strings.Join(argv, " "), err, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("review-state.sh --from called gh\n%s", out)
	}
	return string(out)
}

// outputLine returns the output line that starts with prefix, without it.
func outputLine(t *testing.T, out, prefix string) string {
	t.Helper()
	for l := range strings.SplitSeq(out, "\n") {
		if rest, ok := strings.CutPrefix(l, prefix); ok {
			return rest
		}
	}
	t.Fatalf("no %q line in:\n%s", prefix, out)
	return ""
}

// The countdown and the addressed rules (docs/Development Conventions.md
// § Review state labels), each against a saved GraphQL response.
func TestReviewStateLabels(t *testing.T) {
	cases := []struct {
		name, fixture string
		args          []string
		labels        string // the label set it would apply
		add, remove   string
		also          []string // more output it must contain
	}{
		{
			name:    "clean pass on the head ends the countdown",
			fixture: "clean-pass-on-head",
			labels:  "reviewed-work review-passes-left-0",
			add:     "review-passes-left-0", remove: "review-passes-left-2",
			also: []string{"IC_clean at aaaaaaa: clean, addressed", "gate: met"},
		},
		{
			name:    "a push after the clean pass reopens it: one more pass",
			fixture: "clean-pass-then-push",
			labels:  "reviewed-work review-passes-left-1",
			add:     "review-passes-left-1", remove: "review-passes-left-0",
		},
		{
			// #539's second review: the stale clean pass used to count toward
			// expected minus addressed, so this gave 0 on unreviewed code.
			name:    "a push after a clean pass reopens it whatever passes came before",
			fixture: "addressed-then-clean-then-push",
			labels:  "reviewed-work review-passes-left-1",
			add:     "review-passes-left-1", remove: "review-passes-left-0",
			also: []string{"PRR_one at aaaaaaa: 1 thread, addressed", "gate: not met"},
		},
		{
			name:    "an addressed pass after the push ends it again",
			fixture: "clean-then-push-then-pass",
			labels:  "reviewed-work review-passes-left-0",
			add:     "(none)", remove: "(none)",
			also: []string{"PRR_two at bbbbbbb: 1 thread, addressed", "gate: met"},
		},
		{
			name:    "two addressed passes with findings",
			fixture: "two-addressed-passes",
			labels:  "reviewed-work review-passes-left-0",
			add:     "review-passes-left-0", remove: "review-unaddressed review-passes-left-1",
			also: []string{
				"PRR_one at aaaaaaa: 1 thread, addressed",
				"PRR_two at bbbbbbb: 1 thread, addressed", // replied to, left open
				"IC_verdict: not-ready, addressed",        // the verdict-reply marker
			},
		},
		{
			name:    "unanswered thread and verdict, small issue",
			fixture: "unaddressed-small",
			labels:  "reviewed-work review-unaddressed review-passes-left-1",
			add:     "reviewed-work review-unaddressed review-passes-left-1", remove: "(none)",
			also: []string{"expected /code-review passes: 1 (#902 small)", "since-verdict: 0 "},
		},
		{
			name:    "--clean-pass after a push ends the countdown again",
			fixture: "clean-pass-then-push",
			args:    []string{"--clean-pass"},
			labels:  "reviewed-work review-passes-left-0",
			add:     "(none)", remove: "(none)",
			also: []string{"would post a clean-pass comment at bbbbbbbbbbbb"},
		},
		{
			// What the sync would set before #539's review: a web-UI review
			// comment with no threads used to count as a clean pass.
			name:    "an operator's body-only review is not a pass",
			fixture: "body-only-review",
			labels:  "reviewed-work review-passes-left-2",
			add:     "review-passes-left-2", remove: "review-passes-left-0",
			also: []string{"gate: not met"},
		},
		{
			name:    "one run posting each finding as its own comment is one pass",
			fixture: "standalone-comments",
			labels:  "reviewed-work review-passes-left-1",
			add:     "review-passes-left-1", remove: "review-passes-left-2",
			also: []string{"PRR_f1 +1 more at aaaaaaa: 2 threads, addressed"},
		},
		{
			// #539's review: the merge used to join two /code-review runs on
			// one head into one pass.
			name:    "two runs on one head minutes apart are two passes",
			fixture: "two-runs-same-head",
			labels:  "reviewed-work review-passes-left-0",
			add:     "review-passes-left-0", remove: "review-passes-left-2",
			also: []string{"PRR_f1 at aaaaaaa: 1 thread, addressed", "PRR_f2 at aaaaaaa: 1 thread, addressed"},
		},
		{
			// #539's review: any later comment used to address a thread.
			name:    "a comment after the /address-feedback reply makes its thread unaddressed",
			fixture: "later-comment-reopens",
			labels:  "reviewed-work review-unaddressed review-passes-left-2",
			add:     "review-unaddressed", remove: "(none)",
			also: []string{"PRR_f1 +1 more at aaaaaaa: 2 threads, unaddressed"},
		},
		{
			// #539's review: GitHub links no closing issues on a PR whose
			// base isn't the default branch, so the count fell back to 2.
			name:    "a stacked PR counts the issues its body closes",
			fixture: "stacked-small",
			labels:  "reviewed-work review-passes-left-0",
			add:     "review-passes-left-0", remove: "review-passes-left-2",
			also: []string{"expected /code-review passes: 1 (#902 small; stacked on eharvey/#901, so from the body)"},
		},
		{
			name:    "markers quoted mid-body are not verdicts, replies or clean passes",
			fixture: "quoted-markers",
			labels:  "review-unaddressed review-passes-left-2",
			add:     "review-unaddressed review-passes-left-2", remove: "(none)",
			also: []string{"PRR_quotes at aaaaaaa: 1 thread, unaddressed"},
		},
		{
			name:    "a read past the bounds says the labels aren't written",
			fixture: "past-bounds",
			labels:  "reviewed-work review-passes-left-0",
			add:     "review-passes-left-0", remove: "review-passes-left-2",
			also: []string{"incomplete: more reviews than one call reads"},
		},
		{
			name:    "a closing issue past its label bound is a read past the bounds",
			fixture: "past-label-bounds",
			labels:  "reviewed-work review-passes-left-0",
			add:     "review-passes-left-0", remove: "review-passes-left-2",
			also: []string{"incomplete: more closing issue labels than one call reads"},
		},
		{
			// #515 as it was on 2026-11-05: one /code-review pass whose two
			// threads were both answered (runtime.go's left open after a
			// push-back), plus two empty-body reply reviews that aren't passes.
			// Its replies predate the address-feedback:reply marker, so the
			// open thread reads as unaddressed.
			name:    "PR 515: thread replies are not passes, an unmarked reply doesn't address an open thread",
			fixture: "pr-515",
			labels:  "review-unaddressed review-passes-left-2",
			add:     "review-unaddressed review-passes-left-2", remove: "(none)",
			also: []string{"PRR_kwDOS-8Uos8AAAABWD1fcg at 0e5df90: 2 threads, unaddressed"},
		},
		{
			// #516 as it was on 2026-11-05: its /review-work verdict was
			// posted as a review before the marker existed, so the verdict
			// heading makes it a verdict, not a clean pass.
			name:    "PR 516: a pre-marker verdict review is a verdict, not a pass",
			fixture: "pr-516",
			labels:  "reviewed-work review-unaddressed review-passes-left-2",
			add:     "reviewed-work review-unaddressed review-passes-left-2", remove: "(none)",
			also: []string{
				"PRR_kwDOS-8Uos8AAAABWAz3JA: not-ready, unaddressed",
				"PRR_kwDOS-8Uos8AAAABWD1BrA at ae7dd44: 4 threads, unaddressed", // one open, pre-marker reply
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := runReviewState(t, c.fixture, c.args...)
			for prefix, want := range map[string]string{"labels: ": c.labels, "add: ": c.add, "remove: ": c.remove} {
				if got := outputLine(t, out, prefix); got != want {
					t.Errorf("%s%q, want %q\n%s", prefix, got, want, out)
				}
			}
			for _, s := range c.also {
				if !strings.Contains(out, s) {
					t.Errorf("output lacks %q:\n%s", s, out)
				}
			}
			if !strings.Contains(out, "dry run: nothing changed.") {
				t.Errorf("--from didn't run as a dry run:\n%s", out)
			}
		})
	}
	if out := runReviewState(t, "pr-515"); strings.Count(out, "  pass ") != 1 {
		t.Errorf("PR 515 has one pass, not its three reviews:\n%s", out)
	}
}

// --head names the commit the caller just pushed. A response still showing
// another head writes nothing and fails, rather than syncing the old head.
func TestReviewStateHeadMismatch(t *testing.T) {
	cmd := exec.Command("./review-state.sh", "900", "--from",
		filepath.Join("testdata", "review-state", "clean-pass-on-head.json"), "--head", strings.Repeat("c", 40))
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "not cccccccccccc; nothing written") {
		t.Fatalf("review-state.sh --head with a stale head: err %v, want a failure naming it\n%s", err, out)
	}
}

// Outside a dry run, --clean-pass must name the reviewed commit, so the marker
// never records whatever head GitHub serves just after a push (#539's review).
// It refuses before reading anything.
func TestReviewStateCleanPassNeedsHead(t *testing.T) {
	out, err := exec.Command("./review-state.sh", "900", "--clean-pass").CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(out), "--clean-pass needs --head") {
		t.Fatalf("review-state.sh --clean-pass without --head: err %v, want exit 2 naming --head\n%s", err, out)
	}
}
