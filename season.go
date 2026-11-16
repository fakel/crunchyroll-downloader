package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type SeasonEpisodes struct {
	Data []SeasonEpisode `json:"data"`
}

type SeasonEpisode struct {
	ID                 string        `json:"id"`
	Versions           []*DubVersion `json:"versions"`
	SeasonNumber       int           `json:"season_number"`
	EpisodeNumber      int           `json:"episode_number"`
	SeriesTitle        string        `json:"series_title"`
	SeriesID           string        `json:"series_id"`
	AudioLocale        string        `json:"audio_locale"`
	Title              string        `json:"title"`
	AvailabilityStarts string        `json:"availability_starts"`
}

func getSeasonEpisodes(contentId string, audio_locale string, sub_locale string) []SeasonEpisode {
	if audio_locale == "" {
		audio_locale = "ja-JP"
	}

	if sub_locale == "" {
		sub_locale = "en-US"
	}

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://www.crunchyroll.com/content/v2/cms/seasons/%s/episodes?preferred_audio_language=%s&locale=%s", contentId, audio_locale, sub_locale), nil)
	if err != nil {
		panic(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:147.0) Gecko/20100101 Firefox/147.0")
	resp, err := DoRequest(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	var episodes SeasonEpisodes
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}
	if err = json.Unmarshal(body, &episodes); err != nil {
		panic(err)
	}

	return episodes.Data
}

type Seasons struct {
	Data []Season `json:"data"`
}

type Season struct {
	ID           string `json:"id"`
	SeasonNumber int    `json:"season_number"`
	Title        string `json:"title"`
}

// seasonKind says how a Crunchyroll "season" should be laid out on disk:
// as a real season, as specials, or as extras.
//
// Crunchyroll models all three the same way, so a show whose listing includes a
// one-off special or a pile of bonus clips otherwise lands them in numbered
// season folders the series doesn't have, and media servers show them as if they
// were regular episodes of it.
type seasonKind int

const (
	seasonRegular seasonKind = iota
	seasonSpecials
	seasonExtras
)

// kind classifies a season from its title, which is where Crunchyroll says what
// it is: "Dr. STONE Special Episode - RYUSUI", "Extras". A regular season is
// titled after the series ("Dr. STONE Season 2"), so these terms don't collide
// with it in practice.
func (s Season) kind() seasonKind {
	title := strings.ToLower(s.Title)
	switch {
	case strings.Contains(title, "extras"), strings.Contains(title, "bonus"):
		return seasonExtras
	case strings.Contains(title, "special"):
		return seasonSpecials
	}
	return seasonRegular
}

// seasonTarget is where a Crunchyroll season's episodes should be filed, which
// is not always Crunchyroll's own season number: see seasonTargets.
type seasonTarget struct {
	kind   seasonKind
	number int
}

// seasonTargets resolves where each season of a series belongs.
//
// Crunchyroll numbers every entry in a series' listing as a season, including
// the one-off specials and the extras, so a show with two specials and four real
// seasons is numbered 1..6 and the real seasons land in Season 01, 02, 04, 05
// with a hole where the specials used to be. Those holes are what -renumber-seasons
// removes: the regular seasons are renumbered 1..n in listing order, so the
// folders are contiguous and match how TMDB/TheTVDB index the show. Specials and
// extras keep their own placement (season 0 and Extras/) and are never counted.
func seasonTargets(seasons []Season) map[string]seasonTarget {
	targets := make(map[string]seasonTarget, len(seasons))
	regular := 0
	for _, season := range seasons {
		kind := season.kind()
		number := season.SeasonNumber
		if kind == seasonRegular {
			regular++
			if renumberSeasons != nil && *renumberSeasons {
				number = regular
			}
		}
		targets[season.ID] = seasonTarget{kind: kind, number: number}
	}
	return targets
}

func getSeasons(contentId string, audioLocale string, subLocale string) []Season {
	if audioLocale == "" {
		audioLocale = "ja-JP"
	}

	if subLocale == "" {
		subLocale = "en-US"
	}

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://www.crunchyroll.com/content/v2/cms/series/%s/seasons?force_locale=&preferred_audio_language=%s&locale=%s", contentId, audioLocale, subLocale), nil)
	if err != nil {
		panic(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:147.0) Gecko/20100101 Firefox/147.0")
	resp, err := DoRequest(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	var seasons Seasons
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}
	if err = json.Unmarshal(body, &seasons); err != nil {
		panic(err)
	}

	return seasons.Data
}
