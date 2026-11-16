package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// webConfig is everything the web interface can change. It is stored as JSON in
// the data directory (mode 0600, since it holds the Crunchyroll cookie and the
// SFTP password) and translated into flags and environment variables for each
// download run, so the download code keeps its existing configuration surface.
type webConfig struct {
	// BinaryPath is the program a job runs. Defaults to the running
	// executable, so a job runs exactly the build that's serving the page.
	BinaryPath string `json:"binaryPath"`

	// DownloadDir is the local target passed as --output-dir. With SFTP
	// configured it's the staging directory an episode lands in before it's
	// uploaded and deleted.
	DownloadDir string `json:"downloadDir"`

	// Destination is "local" (keep files in DownloadDir) or "sftp" (upload
	// each episode and delete it locally).
	Destination string `json:"destination"`

	EtpRt          string `json:"etpRt"`
	VideoQuality   string `json:"videoQuality"`
	AudioQuality   string `json:"audioQuality"`
	AudioLang      string `json:"audioLang"`
	SubsLang       string `json:"subsLang"`
	CCLang         string `json:"ccLang"`
	DownloadDelay  string `json:"downloadDelay"`
	AbsoluteNum    bool   `json:"absoluteNumbering"`
	RenumberSeason bool   `json:"renumberSeasons"`

	SFTP sftpConfig `json:"sftp"`

	// Listen is the address the web server binds. Empty means 127.0.0.1:8080.
	Listen string `json:"listen"`
}

// sftpConfig mirrors the CRDL_SFTP_* variables documented in the README.
type sftpConfig struct {
	Host       string `json:"host"`
	Port       string `json:"port"`
	User       string `json:"user"`
	Password   string `json:"password"`
	KeyFile    string `json:"keyFile"`
	RemoteDir  string `json:"remoteDir"`
	KnownHosts string `json:"knownHosts"`
	KeepLocal  bool   `json:"keepLocal"`
}

const (
	destinationLocal = "local"
	destinationSFTP  = "sftp"
)

// defaultConfig returns the settings a fresh install starts with. The defaults
// match the CLI's flag defaults so the two agree.
func defaultConfig() webConfig {
	cfg := webConfig{
		DownloadDir:   filepath.Join("/srv", "downloads"),
		Destination:   destinationLocal,
		VideoQuality:  "1080p",
		AudioQuality:  "192k",
		AudioLang:     "ja-JP",
		SubsLang:      "es-419,en-US",
		CCLang:        "",
		DownloadDelay: "40s",
	}
	if exe, err := os.Executable(); err == nil {
		cfg.BinaryPath = exe
	}
	return cfg
}

// configPath is where the settings live inside the data directory.
func configPath(dataDir string) string {
	return filepath.Join(dataDir, "config.json")
}

// loadWebConfig reads the settings from dataDir, filling in defaults for
// anything missing (a fresh install has no file at all, which is not an error).
func loadWebConfig(dataDir string) (webConfig, error) {
	cfg := defaultConfig()

	body, err := os.ReadFile(configPath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return defaultConfig(), fmt.Errorf("%s: %w", configPath(dataDir), err)
	}
	cfg.applyDefaults()
	return cfg, nil
}

// applyDefaults fills in anything a hand-edited or older file left empty, so a
// missing field can't produce a job with an empty quality or delay.
func (c *webConfig) applyDefaults() {
	def := defaultConfig()
	if c.BinaryPath == "" {
		c.BinaryPath = def.BinaryPath
	}
	if c.DownloadDir == "" {
		c.DownloadDir = def.DownloadDir
	}
	if c.Destination != destinationSFTP {
		c.Destination = destinationLocal
	}
	if c.VideoQuality == "" {
		c.VideoQuality = def.VideoQuality
	}
	if c.AudioQuality == "" {
		c.AudioQuality = def.AudioQuality
	}
	if c.AudioLang == "" {
		c.AudioLang = def.AudioLang
	}
	if c.SubsLang == "" {
		c.SubsLang = def.SubsLang
	}
	if c.DownloadDelay == "" {
		c.DownloadDelay = def.DownloadDelay
	}
	if c.SFTP.Port == "" {
		c.SFTP.Port = "22"
	}
	if c.SFTP.RemoteDir == "" {
		c.SFTP.RemoteDir = "/"
	}
}

// save writes the settings atomically with 0600, because it holds secrets.
func (c webConfig) save(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(configPath(dataDir), append(body, '\n'), 0o600)
}

// validate rejects settings that would make every job fail, so the mistake is
// reported when it's saved rather than as a failed download minutes later.
func (c webConfig) validate() error {
	if c.Destination == destinationSFTP {
		if strings.TrimSpace(c.SFTP.Host) == "" {
			return errors.New("destination is SFTP but no host is set")
		}
		if c.SFTP.Password == "" && c.SFTP.KeyFile == "" {
			return errors.New("destination is SFTP but neither a password nor a key file is set")
		}
	}
	if c.DownloadDir == "" {
		return errors.New("download directory is empty")
	}
	if c.DownloadDelay != "" {
		if _, err := time.ParseDuration(c.DownloadDelay); err != nil {
			return fmt.Errorf("download delay %q: %w", c.DownloadDelay, err)
		}
	}
	return nil
}

// publicView is the settings as the browser sees them: the secrets are replaced
// by a boolean, so the page can say whether one is set without receiving it.
type publicConfig struct {
	BinaryPath     string           `json:"binaryPath"`
	DownloadDir    string           `json:"downloadDir"`
	Destination    string           `json:"destination"`
	VideoQuality   string           `json:"videoQuality"`
	AudioQuality   string           `json:"audioQuality"`
	AudioLang      string           `json:"audioLang"`
	SubsLang       string           `json:"subsLang"`
	CCLang         string           `json:"ccLang"`
	DownloadDelay  string           `json:"downloadDelay"`
	AbsoluteNum    bool             `json:"absoluteNumbering"`
	RenumberSeason bool             `json:"renumberSeasons"`
	HasEtpRt       bool             `json:"hasEtpRt"`
	SFTP           publicSFTPConfig `json:"sftp"`
}

type publicSFTPConfig struct {
	Host       string `json:"host"`
	Port       string `json:"port"`
	User       string `json:"user"`
	KeyFile    string `json:"keyFile"`
	RemoteDir  string `json:"remoteDir"`
	KnownHosts string `json:"knownHosts"`
	KeepLocal  bool   `json:"keepLocal"`
	HasSecret  bool   `json:"hasSecret"`
}

// public hides the cookie and the SFTP password.
func (c webConfig) public() publicConfig {
	return publicConfig{
		BinaryPath:     c.BinaryPath,
		DownloadDir:    c.DownloadDir,
		Destination:    c.Destination,
		VideoQuality:   c.VideoQuality,
		AudioQuality:   c.AudioQuality,
		AudioLang:      c.AudioLang,
		SubsLang:       c.SubsLang,
		CCLang:         c.CCLang,
		DownloadDelay:  c.DownloadDelay,
		AbsoluteNum:    c.AbsoluteNum,
		RenumberSeason: c.RenumberSeason,
		HasEtpRt:       c.EtpRt != "",
		SFTP: publicSFTPConfig{
			Host:       c.SFTP.Host,
			Port:       c.SFTP.Port,
			User:       c.SFTP.User,
			KeyFile:    c.SFTP.KeyFile,
			RemoteDir:  c.SFTP.RemoteDir,
			KnownHosts: c.SFTP.KnownHosts,
			KeepLocal:  c.SFTP.KeepLocal,
			HasSecret:  c.SFTP.Password != "",
		},
	}
}

// mergeUpdate applies a settings PUT to the current settings. Only the keys the
// request actually carries change, which is what makes a partial save safe: a
// field the page didn't mention keeps its value, and a secret it never receives
// is left alone instead of being erased.
func (c webConfig) mergeUpdate(body []byte) (webConfig, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return c, err
	}

	for key, raw := range fields {
		var err error
		switch key {
		case "binaryPath":
			err = json.Unmarshal(raw, &c.BinaryPath)
		case "downloadDir":
			err = json.Unmarshal(raw, &c.DownloadDir)
		case "destination":
			err = json.Unmarshal(raw, &c.Destination)
		case "etpRt":
			err = json.Unmarshal(raw, &c.EtpRt)
		case "videoQuality":
			err = json.Unmarshal(raw, &c.VideoQuality)
		case "audioQuality":
			err = json.Unmarshal(raw, &c.AudioQuality)
		case "audioLang":
			err = json.Unmarshal(raw, &c.AudioLang)
		case "subsLang":
			err = json.Unmarshal(raw, &c.SubsLang)
		case "ccLang":
			err = json.Unmarshal(raw, &c.CCLang)
		case "downloadDelay":
			err = json.Unmarshal(raw, &c.DownloadDelay)
		case "absoluteNumbering":
			err = json.Unmarshal(raw, &c.AbsoluteNum)
		case "renumberSeasons":
			err = json.Unmarshal(raw, &c.RenumberSeason)
		case "listen":
			err = json.Unmarshal(raw, &c.Listen)
		case "sftpPassword":
			err = json.Unmarshal(raw, &c.SFTP.Password)
		case "sftp":
			err = c.SFTP.merge(raw)
		default:
			// Unknown keys are ignored, so an older page keeps working against a
			// newer server.
		}
		if err != nil {
			return c, fmt.Errorf("%s: %w", key, err)
		}
	}
	c.applyDefaults()
	return c, nil
}

// merge applies a partial SFTP update, leaving the password alone unless the
// request carries a new one.
func (s *sftpConfig) merge(raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for key, value := range fields {
		var err error
		switch key {
		case "host":
			err = json.Unmarshal(value, &s.Host)
		case "port":
			err = json.Unmarshal(value, &s.Port)
		case "user":
			err = json.Unmarshal(value, &s.User)
		case "password":
			err = json.Unmarshal(value, &s.Password)
		case "keyFile":
			err = json.Unmarshal(value, &s.KeyFile)
		case "remoteDir":
			err = json.Unmarshal(value, &s.RemoteDir)
		case "knownHosts":
			err = json.Unmarshal(value, &s.KnownHosts)
		case "keepLocal":
			err = json.Unmarshal(value, &s.KeepLocal)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return nil
}

// childEnv is the environment a download job runs with: the server's own
// environment minus the CRDL_* variables it manages here, plus the values from
// the settings. Inheriting the rest keeps PATH (for ffmpeg) and HOME working.
func (c webConfig) childEnv(extra ...string) []string {
	managed := map[string]bool{
		envSFTPHost:       true,
		envSFTPPort:       true,
		envSFTPUser:       true,
		envSFTPPassword:   true,
		envSFTPKey:        true,
		envSFTPRemoteDir:  true,
		envSFTPKeepLocal:  true,
		envSFTPKnownHosts: true,
		"CRDL_ETP_RT":     true,
		"CRDL_PROGRESS":   true,
	}

	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !managed[name] {
			env = append(env, kv)
		}
	}

	// A job's output is a log file, never a terminal.
	env = append(env, "CRDL_PROGRESS=plain")
	if c.EtpRt != "" {
		env = append(env, "CRDL_ETP_RT="+c.EtpRt)
	}
	if c.Destination == destinationSFTP {
		env = append(env,
			envSFTPHost+"="+c.SFTP.Host,
			envSFTPPort+"="+c.SFTP.Port,
			envSFTPUser+"="+c.SFTP.User,
			envSFTPRemoteDir+"="+c.SFTP.RemoteDir,
		)
		if c.SFTP.Password != "" {
			env = append(env, envSFTPPassword+"="+c.SFTP.Password)
		}
		if c.SFTP.KeyFile != "" {
			env = append(env, envSFTPKey+"="+c.SFTP.KeyFile)
		}
		if c.SFTP.KnownHosts != "" {
			env = append(env, envSFTPKnownHosts+"="+c.SFTP.KnownHosts)
		}
		if c.SFTP.KeepLocal {
			env = append(env, envSFTPKeepLocal+"=1")
		}
	}
	return append(env, extra...)
}
