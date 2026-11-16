package main

import (
	"strings"
	"testing"
)

// TestInputArgsIgnoreEditList guards the A/V sync fix.
//
// The audio track Crunchyroll ships declares its edit list media_time in the
// period's 10^7 timescale while its own track timescale is 48 kHz, so the value
// 426667 is read as a media time of 0.0427s by a correct reader but as 8.889s by
// FFmpeg. FFmpeg then aligns the separate inputs by those declared start times
// and pushes the picture almost nine seconds forward, so the finished episode
// plays with the audio far ahead of the image. -ignore_editlist makes each input
// report its own first packet instead, which leaves one frame of residual offset
// (the codec priming samples). Losing the flag on any input silently desyncs
// every download again, which is far subtler than a hard failure: hence the test.
//
// The behaviour itself was verified against a real episode (merging the same
// tracks with and without the flag and comparing the first packet of each
// stream against the subtitle timing); a synthetic file cannot reproduce it,
// because FFmpeg's own muxer never writes an edit list into a fragmented MP4.
func TestInputArgsIgnoreEditList(t *testing.T) {
	got := strings.Join(inputArgs("/tmp/episode/video.mp4"), " ")
	want := "-ignore_editlist 1 -i /tmp/episode/video.mp4"
	if got != want {
		t.Fatalf("inputArgs = %q, want %q", got, want)
	}
}
