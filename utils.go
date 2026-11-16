package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var languageNames = map[string]string{
	"ja-JP":  "日本語",
	"en-US":  "English",
	"en-IN":  "English (India)",
	"id-ID":  "Bahasa Indonesia",
	"ms-MY":  "Bahasa Melayu",
	"ca-ES":  "Català",
	"de-DE":  "Deutsch",
	"es-419": "Español (América Latina)",
	"es-ES":  "Español (España)",
	"fr-FR":  "Français",
	"it-IT":  "Italiano",
	"pl-PL":  "Polski",
	"pt-BR":  "Português (Brasil)",
	"pt-PT":  "Português (Portugal)",
	"vi-VN":  "Tiếng Việt",
	"tr-TR":  "Türkçe",
	"ru-RU":  "Русский",
	"ar-SA":  "العربية",
	"hi-IN":  "हिंदी",
	"ta-IN":  "தமிழ்",
	"te-IN":  "తెలుగు",
	"zh-CN":  "中文 (普通话)",
	"zh-HK":  "中文 (粵語)",
	"zh-TW":  "中文 (國語)",
	"ko-KR":  "한국어",
	"th-TH":  "ไทย",
}

// languageCodes maps a Crunchyroll locale to its ISO 639-2 code, used for the
// ffmpeg "language" stream tag so media servers can select tracks correctly.
var languageCodes = map[string]string{
	"ja-JP":  "jpn",
	"en-US":  "eng",
	"en-IN":  "eng",
	"id-ID":  "ind",
	"ms-MY":  "msa",
	"ca-ES":  "cat",
	"de-DE":  "deu",
	"es-419": "spa",
	"es-ES":  "spa",
	"fr-FR":  "fra",
	"it-IT":  "ita",
	"pl-PL":  "pol",
	"pt-BR":  "por",
	"pt-PT":  "por",
	"vi-VN":  "vie",
	"tr-TR":  "tur",
	"ru-RU":  "rus",
	"ar-SA":  "ara",
	"hi-IN":  "hin",
	"ta-IN":  "tam",
	"te-IN":  "tel",
	"zh-CN":  "zho",
	"zh-HK":  "zho",
	"zh-TW":  "zho",
	"ko-KR":  "kor",
	"th-TH":  "tha",
}

var (
	// Characters that are illegal in Windows filenames or break the final path.
	illegalFilenameChars = regexp.MustCompile(`[\\/:*?"<>|'’\x60“”]`)
	underscoreRuns       = regexp.MustCompile(`_+`)
)

// sanitizeFilename replaces characters that cannot appear in a filename with
// underscores, collapses the resulting runs, and trims trailing spaces and dots
// that Windows silently strips. Empty input yields "Unknown".
func sanitizeFilename(name string) string {
	if name == "" {
		return "Unknown"
	}
	res := illegalFilenameChars.ReplaceAllString(name, "_")
	res = underscoreRuns.ReplaceAllString(res, "_")
	return strings.TrimRight(res, " .")
}

// writeFileAtomic writes body to path via a temporary file in the same
// directory and a rename, so a crash mid-write can never leave a truncated file
// where the program expects JSON.
func writeFileAtomic(path string, body []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename below succeeds

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		return err
	}
	return os.Rename(name, path)
}
