package kvstore

import (
	"go/build"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The README states how many lines of Go ship. That number was typed, and by
// the time a reviewer counted it, it was wrong by three: a guard was added in
// one commit and the sentence was not updated in the next. Every other number
// this repository prints is either measured by a harness that is committed or
// asserted by a test, and the argument for that applies to this one too, so
// here it is.
//
// The count is over the linux/amd64 build set, because that is the platform
// the README calls authoritative for every durability claim in it and the
// number genuinely differs by platform - syncdir_unix.go does the work that
// two stubs decline to do elsewhere. go/build selects the files, so the build
// constraints are evaluated by the same package the toolchain uses rather than
// by a rule reimplemented here, and the answer is the same on any machine that
// runs the test.
func TestTheLineCountInTheReadmeIsTheLineCountOfTheTree(t *testing.T) {
	files, total := shippingLineCount(t)

	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	// The sentence is wrapped, so match against the text with its runs of
	// whitespace flattened rather than against the file as it is laid out.
	flat := strings.Join(strings.Fields(string(readme)), " ")
	m := regexp.MustCompile(`([0-9]+) non-blank, non-comment lines of Go`).FindStringSubmatch(flat)
	if m == nil {
		t.Fatal(`README.md no longer contains a sentence of the form "N non-blank, non-comment lines of Go". If the claim has been dropped, drop this test with it; if it has been reworded, this test has stopped checking anything and that is worse than the drift it exists to catch`)
	}
	claimed, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("README.md claims %q lines: %v", m[1], err)
	}

	if claimed != total {
		t.Errorf("README.md says %d non-blank, non-comment lines of Go; the linux/amd64 build set is %d, over %d files: %s",
			claimed, total, len(files), strings.Join(files, " "))
	}
}

// shippingLineCount counts the way the number was first counted: drop blank
// lines, drop lines whose first non-space characters are "//", count the rest.
//
// That method does not understand /* */, and this package does not use it. The
// test says so out loud and checks it, because a counting method that has
// quietly stopped matching its own description is the exact shape of failure
// this repository is about.
func shippingLineCount(t *testing.T) ([]string, int) {
	t.Helper()

	ctx := build.Default
	ctx.GOOS = "linux"
	ctx.GOARCH = "amd64"
	pkg, err := ctx.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("selecting the linux/amd64 build set with go/build: %v", err)
	}
	if len(pkg.GoFiles) < 5 {
		t.Fatalf("go/build selected %v for linux/amd64, which is too few files to be this package", pkg.GoFiles)
	}

	total := 0
	for _, name := range pkg.GoFiles {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if strings.Contains(string(raw), "/*") {
			t.Errorf("%s contains a /* comment, which the counting method here does not understand - either the method or this line has to change", name)
		}
		for line := range strings.SplitSeq(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "//") {
				continue
			}
			total++
		}
	}
	return pkg.GoFiles, total
}
