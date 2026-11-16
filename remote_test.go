package main

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh/knownhosts"
)

func TestRemoteFromEnvDisabledByDefault(t *testing.T) {
	t.Setenv(envSFTPHost, "")
	if r := remoteFromEnv(); r != nil {
		t.Fatalf("remoteFromEnv() = %+v, want nil without %s", r, envSFTPHost)
	}
}

func TestRemoteFromEnvDefaults(t *testing.T) {
	t.Setenv(envSFTPHost, "192.168.8.2")
	t.Setenv(envSFTPPort, "")
	t.Setenv(envSFTPUser, "")
	t.Setenv(envSFTPRemoteDir, "")
	t.Setenv(envSFTPKeepLocal, "")

	r := remoteFromEnv()
	if r == nil {
		t.Fatal("remoteFromEnv() = nil, want an uploader")
	}
	if r.port != "22" || r.user != "root" || r.remoteDir != "/" {
		t.Errorf("defaults = %+v, want port 22 / root / /", r)
	}
	if r.keepLocal {
		t.Error("keepLocal = true, want false by default")
	}
}

func TestRemoteFromEnvKeepLocalValues(t *testing.T) {
	t.Setenv(envSFTPHost, "h")
	for _, v := range []string{"1", "true", "TRUE", "yes"} {
		t.Setenv(envSFTPKeepLocal, v)
		if r := remoteFromEnv(); !r.keepLocal {
			t.Errorf("keepLocal = false for %q, want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no"} {
		t.Setenv(envSFTPKeepLocal, v)
		if r := remoteFromEnv(); r.keepLocal {
			t.Errorf("keepLocal = true for %q, want false", v)
		}
	}
}

// TestRemotePath mirrors the Jellyfin tree under the remote directory so the
// server doesn't need any re-organising afterwards.
func TestRemotePath(t *testing.T) {
	r := &remoteUploader{remoteDir: "/boveda/multimedia/series"}
	base := "/home/magneto/Videos"
	local := path.Join(base, "Cowboy Bebop (1998)", "Season 01",
		"Cowboy Bebop (1998) S01E05 - Ballad of Fallen Angels [720p].mkv")

	got, err := r.remotePath(local, base)
	if err != nil {
		t.Fatal(err)
	}
	want := "/boveda/multimedia/series/Cowboy Bebop (1998)/Season 01/Cowboy Bebop (1998) S01E05 - Ballad of Fallen Angels [720p].mkv"
	if got != want {
		t.Errorf("remotePath()\n got: %q\nwant: %q", got, want)
	}
}

func TestRemotePathRejectsOutsideBase(t *testing.T) {
	r := &remoteUploader{remoteDir: "/srv"}
	if _, err := r.remotePath("/elsewhere/a.mkv", "/home/magneto/Videos"); err == nil {
		t.Error("remotePath() accepted a file outside the base directory")
	}
}

func TestRemoteFromEnvKnownHosts(t *testing.T) {
	t.Setenv(envSFTPHost, "h")
	t.Setenv(envSFTPKnownHosts, " /etc/ssh/known_hosts ")
	if r := remoteFromEnv(); r.knownHosts != "/etc/ssh/known_hosts" {
		t.Errorf("knownHosts = %q, want it trimmed", r.knownHosts)
	}
}

// Without CRDL_SFTP_KNOWN_HOSTS, check() must stay quiet: verification is
// opt-in, not a startup requirement.
func TestCheckSkippedWithoutKnownHosts(t *testing.T) {
	r := &remoteUploader{host: "h"}
	if err := r.check(); err != nil {
		t.Errorf("check() = %v, want nil", err)
	}
	cb, err := r.hostKeyCallback()
	if err != nil || cb == nil {
		t.Fatalf("hostKeyCallback() = %v, %v; want an insecure callback", cb, err)
	}
}

func TestCheckFailsOnMissingKnownHosts(t *testing.T) {
	r := &remoteUploader{host: "h", knownHosts: filepath.Join(t.TempDir(), "nope")}
	if err := r.check(); err == nil {
		t.Error("check() = nil, want an error for an unreadable known_hosts")
	}
}

func TestCheckAcceptsRealKnownHosts(t *testing.T) {
	// A syntactically valid entry: knownhosts.New only parses the file here.
	// Whether the live server matches it is verified by the e2e run, not a test.
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	file := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(file, []byte("[192.0.2.10]:22 "+key+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := &remoteUploader{host: "192.0.2.10", port: "22", knownHosts: file}
	if err := r.check(); err != nil {
		t.Fatalf("check() = %v, want nil", err)
	}
	if _, err := r.hostKeyCallback(); err != nil {
		t.Fatalf("hostKeyCallback() = %v, want nil", err)
	}
}

// A host key mismatch must name the fix instead of surfacing the bare handshake
// error, because "ssh: handshake failed" is what makes people disable the check.
func TestDescribeDialError(t *testing.T) {
	r := &remoteUploader{host: "192.168.8.2", port: "22", knownHosts: "/home/u/.ssh/known_hosts"}

	t.Run("mismatch suggests the keygen command", func(t *testing.T) {
		err := describeDialError(&knownhosts.KeyError{Want: []knownhosts.KnownKey{{}}}, r)
		if !strings.Contains(err.Error(), "ssh-keygen -R 192.168.8.2:22") {
			t.Errorf("error = %q, want a ssh-keygen -R hint", err)
		}
	})

	t.Run("unknown host suggests ssh-keyscan", func(t *testing.T) {
		err := describeDialError(&knownhosts.KeyError{}, r)
		want := "ssh-keyscan -p 22 192.168.8.2 >> /home/u/.ssh/known_hosts"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q", err, want)
		}
	})

	t.Run("other errors are passed through", func(t *testing.T) {
		original := errors.New("dial tcp: connection refused")
		if got := describeDialError(original, r); got != original {
			t.Errorf("describeDialError() = %v, want the original error", got)
		}
	})
}
