package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestProgressBarRenderLine(t *testing.T) {
	bar := &progressBar{title: "video", label: "segments", total: 100, current: 42}
	line := bar.renderLine(displayWidth("video"))
	if !strings.Contains(line, "42%") {
		t.Fatalf("renderLine() = %q, want it to include the percentage", line)
	}
	if !strings.Contains(line, "(42/100 segments)") {
		t.Fatalf("renderLine() = %q, want it to include the segment count", line)
	}
	if !strings.HasPrefix(line, "video: ") {
		t.Fatalf("renderLine() = %q, want it prefixed with the title", line)
	}
}

func TestProgressBarRenderLineClampsAndHidesDetail(t *testing.T) {
	bar := &progressBar{title: "video", label: "segments", total: 100, current: 150}
	line := bar.renderLine(displayWidth("video"))
	if !strings.Contains(line, "100%") || !strings.Contains(line, "(100/100 segments)") {
		t.Fatalf("renderLine() = %q, want it clamped to 100%%", line)
	}

	bar = &progressBar{total: 100, current: 50}
	if line := bar.renderLine(0); !strings.HasSuffix(line, "50%") {
		t.Fatalf("renderLine() = %q, want it to omit label and title when unset", line)
	}
}

func TestProgressBarRenderLineAlignsTitles(t *testing.T) {
	bars := []*progressBar{
		{title: "Downloading video"},
		{title: "Downloading English audio"},
		{title: "Downloading 日本語 audio"},
	}
	titleWidth := 0
	for _, b := range bars {
		if w := displayWidth(b.title); w > titleWidth {
			titleWidth = w
		}
	}

	for _, b := range bars {
		line := b.renderLine(titleWidth)
		idx := strings.Index(line, ": ")
		if idx < 0 {
			t.Fatalf("renderLine(%q) = %q, missing title separator", b.title, line)
		}
		if got := displayWidth(line[:idx]); got != titleWidth {
			t.Fatalf("renderLine(%q) = %q, bar starts at column %d, want %d", b.title, line, got, titleWidth)
		}
	}
}

// TestPlainProgressLine pins the format the web interface parses: one stable
// line per update instead of an animated bar, since the child's output is a log
// file there.
func TestPlainProgressLine(t *testing.T) {
	old := plainProgress
	plainProgress = true
	defer func() { plainProgress = old }()

	// Capture stdout while the bar renders.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = stdout }()

	b := newProgressBar("video", 100, "segments")
	for i := int64(1); i <= 5; i++ {
		b.update(i*20, i*1000)
	}
	b.finish()
	w.Close()
	os.Stdout = stdout

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "progress: video ") {
		t.Fatalf("plain output = %q, want lines starting with %q", string(out), "progress: video ")
	}
	for _, line := range lines {
		if strings.Contains(line, "\x1b") {
			t.Errorf("plain progress line contains a cursor escape: %q", line)
		}
	}
	if last := lines[len(lines)-1]; last != "progress: video 100% done" {
		t.Errorf("last line = %q, want %q", last, "progress: video 100% done")
	}
}
