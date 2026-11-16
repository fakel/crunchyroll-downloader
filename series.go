package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// seriesInfo is the subset of Crunchyroll's series metadata needed to lay files
// out in a way media servers (Jellyfin) can match against their own databases.
// The launch year disambiguates remakes that share a title.
type seriesInfo struct {
	Title      string
	LaunchYear int
}

type seriesResponse struct {
	Data []struct {
		Title            string `json:"title"`
		SeriesLaunchYear int    `json:"series_launch_year"`
	} `json:"data"`
}

// seriesCache holds one lookup per series per run: a whole-season download asks
// for the same series ID once per episode.
var seriesCache = map[string]seriesInfo{}

// getSeriesInfo fetches the series title and launch year, returning a zero
// seriesInfo (rather than failing) if Crunchyroll doesn't provide them. The
// year is cosmetic: a missing year only costs a slightly worse Jellyfin match.
func getSeriesInfo(seriesID string) seriesInfo {
	if info, ok := seriesCache[seriesID]; ok {
		return info
	}
	if seriesID == "" {
		return seriesInfo{}
	}

	// The empty force_locale/preferred_audio_language params are required; the
	// endpoint rejects the request without them.
	url := fmt.Sprintf("https://www.crunchyroll.com/content/v2/cms/series/%s?force_locale=&preferred_audio_language=ja-JP&locale=en-US", seriesID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return seriesInfo{}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:147.0) Gecko/20100101 Firefox/147.0")
	resp, err := DoRequest(req)
	if err != nil {
		return seriesInfo{}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return seriesInfo{}
	}
	var res seriesResponse
	if err = json.Unmarshal(body, &res); err != nil || len(res.Data) == 0 {
		return seriesInfo{}
	}

	info := seriesInfo{Title: res.Data[0].Title, LaunchYear: res.Data[0].SeriesLaunchYear}
	seriesCache[seriesID] = info
	return info
}
