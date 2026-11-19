package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Environment variables that configure the optional remote destination. Setting
// CRDL_SFTP_HOST is what turns the feature on; everything else has a sane
// default so the common case is just host + password.
const (
	envSFTPHost       = "CRDL_SFTP_HOST"
	envSFTPPort       = "CRDL_SFTP_PORT"
	envSFTPUser       = "CRDL_SFTP_USER"
	envSFTPPassword   = "CRDL_SFTP_PASSWORD"
	envSFTPKey        = "CRDL_SFTP_KEY"
	envSFTPRemoteDir  = "CRDL_SFTP_REMOTE_DIR"
	envSFTPKeepLocal  = "CRDL_SFTP_KEEP_LOCAL"
	envSFTPKnownHosts = "CRDL_SFTP_KNOWN_HOSTS"
)

// remoteUploader copies each finished episode to an SFTP server and then frees
// the local copy, so a whole season never has to fit on the local disk. It is
// nil when the feature isn't configured, which makes every method below a no-op
// at the call site.
type remoteUploader struct {
	host      string
	port      string
	user      string
	password  string
	keyFile   string
	remoteDir string
	keepLocal bool
	// knownHosts enables host key verification. Empty keeps the old behaviour
	// (accept any key), which is fine for a LAN box reached by IP but not for
	// anything else.
	knownHosts string
}

// remoteFromEnv builds the uploader from the environment, or returns nil when
// CRDL_SFTP_HOST is unset.
func remoteFromEnv() *remoteUploader {
	host := strings.TrimSpace(os.Getenv(envSFTPHost))
	if host == "" {
		return nil
	}
	r := &remoteUploader{
		host:       host,
		port:       envOr(envSFTPPort, "22"),
		user:       envOr(envSFTPUser, "root"),
		password:   os.Getenv(envSFTPPassword),
		keyFile:    os.Getenv(envSFTPKey),
		remoteDir:  envOr(envSFTPRemoteDir, "/"),
		knownHosts: strings.TrimSpace(os.Getenv(envSFTPKnownHosts)),
	}
	keep := strings.ToLower(os.Getenv(envSFTPKeepLocal))
	r.keepLocal = keep == "1" || keep == "true" || keep == "yes"
	return r
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// describe returns a short human-readable destination for log lines.
func (r *remoteUploader) describe() string {
	return fmt.Sprintf("%s@%s:%s", r.user, r.host, r.remoteDir)
}

// check validates the configuration up front, so a typo in the known_hosts path
// is reported before a whole season is downloaded instead of at upload time.
func (r *remoteUploader) check() error {
	if r.knownHosts == "" {
		return nil
	}
	if _, err := knownhosts.New(r.knownHosts); err != nil {
		return fmt.Errorf("%s=%s: %w", envSFTPKnownHosts, r.knownHosts, err)
	}
	return nil
}

// hostKeyCallback verifies the server key against known_hosts when configured.
func (r *remoteUploader) hostKeyCallback() (ssh.HostKeyCallback, error) {
	if r.knownHosts == "" {
		// Nothing to verify against. This is the documented default: the target
		// is normally a machine on the local network reached by IP, so there is
		// no stable key to pin and a rejection would only block the download.
		return ssh.InsecureIgnoreHostKey(), nil
	}
	return knownhosts.New(r.knownHosts)
}

// remoteDirectory maps a local output directory to its place under remoteDir.
// baseDirectory is the same output dir the file was written under, so the
// relative part is just the Jellyfin tree. A path outside it is refused rather
// than uploaded as ../../something.
func (r *remoteUploader) remoteDirectory(localDir, baseDirectory string) (string, error) {
	rel, err := filepath.Rel(baseDirectory, localDir)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside %s", localDir, baseDirectory)
	}
	if rel == "." {
		return r.remoteDir, nil
	}
	return path.Join(r.remoteDir, filepath.ToSlash(rel)), nil
}

// remotePath maps a local output file to its place under remoteDir, keeping the
// <Series> (<Year>)/Season NN/ layout that Jellyfin expects.
func (r *remoteUploader) remotePath(localPath, baseDirectory string) (string, error) {
	dir, err := r.remoteDirectory(filepath.Dir(localPath), baseDirectory)
	if err != nil {
		return "", err
	}
	return path.Join(dir, filepath.Base(localPath)), nil
}

// describeDialError turns a host key mismatch into something actionable instead
// of the bare "ssh: handshake failed: knownhosts: key mismatch".
func describeDialError(err error, r *remoteUploader) error {
	var keyErr *knownhosts.KeyError
	if errors.As(err, &keyErr) && len(keyErr.Want) > 0 {
		host := net.JoinHostPort(r.host, r.port)
		return fmt.Errorf("%w (the server's key differs from the entry in %s; if it was reinstalled, run: ssh-keygen -R %s)",
			err, r.knownHosts, host)
	}
	if errors.As(err, &keyErr) {
		return fmt.Errorf("%w (%s has no entry for %s; add one with: ssh-keyscan -p %s %s >> %s)",
			err, r.knownHosts, net.JoinHostPort(r.host, r.port), r.port, r.host, r.knownHosts)
	}
	return err
}

// sftpClient closes the SSH connection as well as the SFTP session.
// sftp.Client.Close only ends the session (conn.go), so without this every
// connect() leaked one TCP connection to the SFTP server — a long season
// left dozens of idle sockets behind.
type sftpClient struct {
	*sftp.Client
	conn *ssh.Client
}

func (c *sftpClient) Close() error {
	err := c.Client.Close()
	_ = c.conn.Close()
	return err
}

func (r *remoteUploader) connect() (*sftpClient, error) {
	auth := []ssh.AuthMethod{}
	if r.keyFile != "" {
		key, err := os.ReadFile(r.keyFile)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", r.keyFile, err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", r.keyFile, err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if r.password != "" {
		auth = append(auth, ssh.Password(r.password))
	}
	if len(auth) == 0 {
		return nil, fmt.Errorf("set %s or %s", envSFTPPassword, envSFTPKey)
	}

	// The server is on the LAN and reached by IP, so there's no stable host key
	// to pin; requiring one would only add a blocking prompt.
	callback, err := r.hostKeyCallback()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envSFTPKnownHosts, err)
	}
	conf := &ssh.ClientConfig{
		User:            r.user,
		Auth:            auth,
		HostKeyCallback: callback,
		Timeout:         30 * time.Second,
	}
	conn, err := ssh.Dial("tcp", net.JoinHostPort(r.host, r.port), conf)
	if err != nil {
		return nil, describeDialError(err, r)
	}
	client, err := sftp.NewClient(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &sftpClient{Client: client, conn: conn}, nil
}

// findEpisode looks inside the remote season directory for a file whose name
// contains marker ("S01E05 - ") and returns its name. Matching on the episode
// marker instead of the whole file name is what keeps a re-run from downloading
// an episode again just because its title changed language or the quality tag
// differs. Reconnecting for the check is cheap next to downloading an episode.
func (r *remoteUploader) findEpisode(localDir, baseDirectory, marker string) (string, bool) {
	dir, err := r.remoteDirectory(localDir, baseDirectory)
	if err != nil {
		return "", false
	}
	client, err := r.connect()
	if err != nil {
		return "", false
	}
	defer client.Close()

	entries, err := client.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.Contains(entry.Name(), marker) {
			return entry.Name(), true
		}
	}
	return "", false
}

// upload copies the file and only then renames it into place, so a partially
// transferred episode is never visible to Jellyfin.
func (r *remoteUploader) upload(localPath, baseDirectory string) error {
	remotePath, err := r.remotePath(localPath, baseDirectory)
	if err != nil {
		return err
	}

	client, err := r.connect()
	if err != nil {
		return err
	}
	defer client.Close()

	if dir := path.Dir(remotePath); dir != "." && dir != "/" {
		if err := client.MkdirAll(dir); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()

	tmpPath := remotePath + ".part"
	dst, err := client.Create(tmpPath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = client.Remove(tmpPath)
		return err
	}
	if err := dst.Close(); err != nil {
		_ = client.Remove(tmpPath)
		return err
	}

	_ = client.Remove(remotePath)
	if err := client.Rename(tmpPath, remotePath); err != nil {
		return fmt.Errorf("rename %s: %w", tmpPath, err)
	}
	return nil
}

// push uploads a finished episode and, unless CRDL_SFTP_KEEP_LOCAL is set,
// deletes the local file to reclaim disk space. Failures are logged and the
// local file is kept: losing an already-downloaded episode is worse than a full
// disk.
func (r *remoteUploader) push(localPath, baseDirectory string) {
	if r == nil {
		return
	}
	fmt.Printf("Uploading to %s...\n", r.describe())
	if err := r.upload(localPath, baseDirectory); err != nil {
		fmt.Printf("! Upload failed, keeping the local file: %v\n", err)
		return
	}
	fmt.Printf("Uploaded %s\n", path.Base(localPath))
	if !r.keepLocal {
		if err := os.Remove(localPath); err != nil {
			fmt.Printf("! Could not remove local file: %v\n", err)
			return
		}
		fmt.Println("Removed the local copy.")
		// The season folder is left behind empty once its last episode is gone;
		// a left-over tree of empty folders makes the local disk look used when
		// it isn't, so drop the folder too (and only if it is empty).
		if err := os.Remove(filepath.Dir(localPath)); err == nil {
			_ = os.Remove(filepath.Dir(filepath.Dir(localPath)))
		}
	}
}
