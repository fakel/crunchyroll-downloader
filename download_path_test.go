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

// TestJellyfinEpisodePathSpecialsAndExtras covers the seasons Crunchyroll lists
// that aren't seasons of the series: a one-off special and a pile of bonus
// clips. Filed as numbered seasons, they show up in Jellyfin as episodes the
// series doesn't have, so they go to Specials/ (season 0) and Extras/ instead.
func TestJellyfinEpisodePathSpecialsAndExtras(t *testing.T) {
	tests := []struct {
		name string
		info EpisodeInfo
		want string
	}{
		{
			name: "special becomes season 0 in Specials/",
			info: EpisodeInfo{
				EpisodeMetadata:  EpisodeMetadata{SeriesTitle: "Dr. STONE", SeasonNumber: 3, EpisodeNumber: 1},
				Title:            "RYUSUI",
				SeriesLaunchYear: 2019,
				SeasonKind:       seasonSpecials,
			},
			want: "Dr. STONE (2019)/Specials/Dr. STONE (2019) S00E01 - RYUSUI [1080p].mkv",
		},
		{
			name: "extras go to Extras/ and keep their own numbering",
			info: EpisodeInfo{
				EpisodeMetadata:  EpisodeMetadata{SeriesTitle: "Dr. STONE", SeasonNumber: 6, EpisodeNumber: 2},
				Title:            "Behind the Scenes",
				SeriesLaunchYear: 2019,
				SeasonKind:       seasonExtras,
			},
			want: "Dr. STONE (2019)/Extras/Dr. STONE (2019) S06E02 - Behind the Scenes [1080p].mkv",
		},
		{
			name: "a regular season is untouched",
			info: EpisodeInfo{
				EpisodeMetadata:  EpisodeMetadata{SeriesTitle: "Dr. STONE", SeasonNumber: 2, EpisodeNumber: 5},
				Title:            "Stone Wars",
				SeriesLaunchYear: 2019,
			},
			want: "Dr. STONE (2019)/Season 02/Dr. STONE (2019) S02E05 - Stone Wars [1080p].mkv",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			got, err := filepath.Rel(base, jellyfinEpisodePath(base, tt.info, "1080p"))
			if err != nil {
				t.Fatalf("relative path: %v", err)
			}
			if got != tt.want {
				t.Errorf("jellyfinEpisodePath()\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

// TestSeasonEpisodeNumbersSpecials keeps -absolute-numbering from dragging a
// special into Season 01: a special is season 0 no matter how the rest of the
// series is numbered.
func TestSeasonEpisodeNumbersSpecials(t *testing.T) {
	info := EpisodeInfo{
		EpisodeMetadata: EpisodeMetadata{SeasonNumber: 3, EpisodeNumber: 1},
		SeasonKind:      seasonSpecials,
	}

	if season, episode := seasonEpisodeNumbers(info); season != 0 || episode != 1 {
		t.Errorf("special = (%d, %d), want (0, 1)", season, episode)
	}

	*absoluteNum = true
	defer func() { *absoluteNum = false }()
	if season, episode := seasonEpisodeNumbers(info); season != 0 || episode != 1 {
		t.Errorf("special with absolute numbering = (%d, %d), want (0, 1)", season, episode)
	}
}

// TestSeasonKind pins the titles Crunchyroll uses for the seasons that aren't
// seasons of the series.
func TestSeasonKind(t *testing.T) {
	tests := []struct {
		title string
		want  seasonKind
	}{
		{"Dr. STONE", seasonRegular},
		{"Dr. STONE Season 2", seasonRegular},
		{"Dr. STONE SCIENCE FUTURE", seasonRegular},
		{"Dr. STONE Special Episode - RYUSUI", seasonSpecials},
		{"Dr. STONE OVA", seasonRegular},
		{"Extras", seasonExtras},
		{"Dr. STONE Bonus Clips", seasonExtras},
	}

	for _, tt := range tests {
		if got := (Season{Title: tt.title}).kind(); got != tt.want {
			t.Errorf("Season(%q).kind() = %v, want %v", tt.title, got, tt.want)
		}
	}
}

// drStoneSeasons is Dr. STONE's listing as Crunchyroll serves it: four real
// seasons, numbered 1, 2, 4 and 5 because a special and a pile of extras sit
// between them in the same list.
func drStoneSeasons() []Season {
	return []Season{
		{ID: "s1", SeasonNumber: 1, Title: "Dr. STONE"},
		{ID: "s2", SeasonNumber: 2, Title: "Dr. STONE Season 2"},
		{ID: "special", SeasonNumber: 3, Title: "Dr. STONE Special Episode - RYUSUI"},
		{ID: "s3", SeasonNumber: 4, Title: "Dr. STONE Season 3"},
		{ID: "s4", SeasonNumber: 5, Title: "Dr. STONE SCIENCE FUTURE"},
		{ID: "extras", SeasonNumber: 6, Title: "Extras"},
	}
}

// TestSeasonTargetsUnchanged keeps the default: Crunchyroll's own numbering.
func TestSeasonTargetsUnchanged(t *testing.T) {
	targets := seasonTargets(drStoneSeasons())

	for id, want := range map[string]seasonTarget{
		"s1":      {kind: seasonRegular, number: 1},
		"s2":      {kind: seasonRegular, number: 2},
		"special": {kind: seasonSpecials, number: 3},
		"s3":      {kind: seasonRegular, number: 4},
		"s4":      {kind: seasonRegular, number: 5},
		"extras":  {kind: seasonExtras, number: 6},
	} {
		if got := targets[id]; got != want {
			t.Errorf("seasonTargets()[%q] = %+v, want %+v", id, got, want)
		}
	}
}

// TestSeasonTargetsRenumbered covers -renumber-seasons: the two seasons after
// the special close the gap, and neither the special nor the extras is counted,
// so the folders are Season 01..04 with no hole.
func TestSeasonTargetsRenumbered(t *testing.T) {
	*renumberSeasons = true
	defer func() { *renumberSeasons = false }()

	targets := seasonTargets(drStoneSeasons())

	for id, want := range map[string]seasonTarget{
		"s1":      {kind: seasonRegular, number: 1},
		"s2":      {kind: seasonRegular, number: 2},
		"special": {kind: seasonSpecials, number: 3},
		"s3":      {kind: seasonRegular, number: 3},
		"s4":      {kind: seasonRegular, number: 4},
		"extras":  {kind: seasonExtras, number: 6},
	} {
		if got := targets[id]; got != want {
			t.Errorf("seasonTargets()[%q] = %+v, want %+v", id, got, want)
		}
	}
}

// TestRenumberedSeasonPath checks the renumbering all the way to the file name
// and the container tag, and that a special is still season 0.
func TestRenumberedSeasonPath(t *testing.T) {
	*renumberSeasons = true
	defer func() { *renumberSeasons = false }()

	base := t.TempDir()
	targets := seasonTargets(drStoneSeasons())

	tests := []struct {
		seasonID string
		episode  int
		title    string
		want     string
	}{
		{"s3", 5, "Stone Wars", "dr stone (2019)/Season 03/dr stone (2019) S03E05 - Stone Wars [1080p].mkv"},
		{"s4", 1, "New World", "dr stone (2019)/Season 04/dr stone (2019) S04E01 - New World [1080p].mkv"},
		{"special", 1, "RYUSUI", "dr stone (2019)/Specials/dr stone (2019) S00E01 - RYUSUI [1080p].mkv"},
	}

	for _, tt := range tests {
		target := targets[tt.seasonID]
		info := EpisodeInfo{
			EpisodeMetadata:      EpisodeMetadata{SeriesTitle: "dr stone", EpisodeNumber: tt.episode, SeasonNumber: target.number},
			Title:                tt.title,
			SeriesLaunchYear:     2019,
			SeasonKind:           target.kind,
			SeasonNumberOverride: target.number,
		}
		got, err := filepath.Rel(base, jellyfinEpisodePath(base, info, "1080p"))
		if err != nil {
			t.Fatalf("relative path: %v", err)
		}
		if got != tt.want {
			t.Errorf("season %q episode %d\n got: %q\nwant: %q", tt.seasonID, tt.episode, got, tt.want)
		}
	}
}
