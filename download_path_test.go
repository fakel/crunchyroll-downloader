package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestJellyfinEpisodePath pins the layout Jellyfin matches episodes from:
// <base>/<Series> (<Year>)/Season NN/<Series> (<Year>) SxxExx - <Title>.mkv
func TestJellyfinEpisodePath(t *testing.T) {
	tests := []struct {
		name string
		info EpisodeInfo
		want string
	}{
		{
			name: "with launch year",
			info: EpisodeInfo{
				EpisodeMetadata:  EpisodeMetadata{SeriesTitle: "One Piece", SeasonNumber: 4, EpisodeNumber: 62},
				Title:            "The First Line of Defense?",
				SeriesLaunchYear: 1999,
			},
			want: "One Piece (1999)/Season 04/One Piece (1999) S04E62 - The First Line of Defense_ [1080p].mkv",
		},
		{
			name: "unknown year still lays out a season folder",
			info: EpisodeInfo{
				EpisodeMetadata: EpisodeMetadata{SeriesTitle: "Some Show", SeasonNumber: 1, EpisodeNumber: 3},
				Title:           "Pilot",
			},
			want: "Some Show/Season 01/Some Show S01E03 - Pilot [1080p].mkv",
		},
		{
			name: "illegal characters are sanitized in both folder and filename",
			info: EpisodeInfo{
				EpisodeMetadata:  EpisodeMetadata{SeriesTitle: "Re:Zero", SeasonNumber: 2, EpisodeNumber: 13},
				Title:            `Who's / There?`,
				SeriesLaunchYear: 2016,
			},
			want: "Re_Zero (2016)/Season 02/Re_Zero (2016) S02E13 - Who_s _ There_ [1080p].mkv",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := jellyfinEpisodePath("/media/tv", tc.info, "1080p")
			want := filepath.Join("/media/tv", filepath.FromSlash(tc.want))
			if got != want {
				t.Errorf("jellyfinEpisodePath()\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

// TestJellyfinEpisodePathAbsolute covers -absolute-numbering: One Piece is a
// single season on TMDB, so Crunchyroll's "Season 4" must become Season 01 with
// the episode's own (already absolute) number.
func TestJellyfinEpisodePathAbsolute(t *testing.T) {
	info := EpisodeInfo{
		EpisodeMetadata:  EpisodeMetadata{SeriesTitle: "One Piece", SeasonNumber: 4, EpisodeNumber: 62},
		Title:            "The First Line of Defense?",
		SeriesLaunchYear: 1999,
	}

	*absoluteNum = true
	defer func() { *absoluteNum = false }()

	got := jellyfinEpisodePath("/media/tv", info, "1080p")
	want := filepath.Join("/media/tv", "One Piece (1999)", "Season 01",
		"One Piece (1999) S01E62 - The First Line of Defense_ [1080p].mkv")
	if got != want {
		t.Errorf("jellyfinEpisodePath() with absolute numbering\n got: %q\nwant: %q", got, want)
	}
}

func TestSeasonEpisodeNumbers(t *testing.T) {
	info := EpisodeInfo{EpisodeMetadata: EpisodeMetadata{SeasonNumber: 4, EpisodeNumber: 62}}

	if season, episode := seasonEpisodeNumbers(info); season != 4 || episode != 62 {
		t.Errorf("default = (%d, %d), want (4, 62)", season, episode)
	}

	*absoluteNum = true
	defer func() { *absoluteNum = false }()
	if season, episode := seasonEpisodeNumbers(info); season != 1 || episode != 62 {
		t.Errorf("absolute = (%d, %d), want (1, 62)", season, episode)
	}
}

// Episode identity is the SxxExx marker, not the file name: a re-run with a
// different --subs-lang titles the file in another language, and that must not
// look like a missing episode.
func TestEpisodeMarker(t *testing.T) {
	info := EpisodeInfo{EpisodeMetadata: EpisodeMetadata{SeasonNumber: 1, EpisodeNumber: 5}}

	if got, want := episodeMarker(info), "S01E05 - "; got != want {
		t.Fatalf("episodeMarker() = %q, want %q", got, want)
	}

	// With absolute numbering the marker has to follow the same season the path
	// uses, or the skip check and the file name would disagree.
	saved := absoluteNum
	defer func() { absoluteNum = saved }()
	truthy := true
	absoluteNum = &truthy
	info.EpisodeMetadata.SeasonNumber = 4
	info.EpisodeMetadata.EpisodeNumber = 62
	if got, want := episodeMarker(info), "S01E62 - "; got != want {
		t.Fatalf("episodeMarker() with absolute numbering = %q, want %q", got, want)
	}
}

func TestFindEpisodeInDir(t *testing.T) {
	dir := t.TempDir()
	marker := "S01E05 - "

	if _, ok := findEpisodeInDir(dir, marker); ok {
		t.Fatal("findEpisodeInDir() found an episode in an empty directory")
	}

	// Same episode, Spanish title and a different quality: still the same episode.
	name := "Cowboy Bebop (1998) S01E05 - La balada de los ángeles caídos [720p].mkv"
	writeFile(t, filepath.Join(dir, name))
	// A subtitle file for the same episode is not the episode.
	writeFile(t, filepath.Join(dir, "Cowboy Bebop (1998) S01E05 - La balada de los ángeles caídos.es.srt"))
	// A neighbouring episode must not match.
	writeFile(t, filepath.Join(dir, "Cowboy Bebop (1998) S01E15 - Ballad of Fallen Angels [720p].mkv"))

	got, ok := findEpisodeInDir(dir, marker)
	if !ok {
		t.Fatal("findEpisodeInDir() = false, want the existing episode")
	}
	if got != name {
		t.Fatalf("findEpisodeInDir() = %q, want %q", got, name)
	}

	if _, ok := findEpisodeInDir(filepath.Join(dir, "missing"), marker); ok {
		t.Fatal("findEpisodeInDir() found an episode in a directory that doesn't exist")
	}
}
