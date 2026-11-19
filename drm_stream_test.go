package main

import (
	"bytes"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/iyear/gowidevine"
	"github.com/iyear/gowidevine/widevinepb"
)

// mp4ffModuleDir locates the mp4ff module cache so the test can reuse its
// encrypted-fixture helpers' testdata without vendoring binary files.
func mp4ffTestdata(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Skipf("go env GOMODCACHE: %v", err)
	}
	dir := filepath.Join(strings.TrimSpace(string(out)), "github.com", "!eyevinn", "mp4ff@v0.63.0", "mp4", "testdata")
	if _, err := os.Stat(filepath.Join(dir, "init.mp4")); err != nil {
		t.Skipf("mp4ff testdata not available: %v", err)
	}
	return dir
}

// TestDecryptMP4StreamingMatchesWholeFile encrypts mp4ff's own init+segment
// fixtures and checks that decryptMP4 (now streaming) produces byte-identical
// output to the old whole-file path (gowidevine DecryptMP4 / DecodeFile).
// This guards the streaming refactor that fixed multi-GB RAM usage.
func TestDecryptMP4StreamingMatchesWholeFile(t *testing.T) {
	dir := mp4ffTestdata(t)
	key, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	iv, _ := hex.DecodeString("7766554433221100")
	kidHex := "11112222333344445555666677778888"
	kidUUID, _ := mp4.NewUUIDFromString(kidHex)
	kid, _ := hex.DecodeString(kidHex)

	ifh, err := os.Open(filepath.Join(dir, "init.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	init, err := mp4.DecodeFile(ifh)
	ifh.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mp4.InitProtect(init.Init, key, iv, "cenc", kidUUID, nil); err != nil {
		t.Fatal(err)
	}
	var encInitBuf bytes.Buffer
	if err := init.Encode(&encInitBuf); err != nil {
		t.Fatal(err)
	}
	encInit := encInitBuf.Bytes()

	rawSeg, err := os.ReadFile(filepath.Join(dir, "1.m4s"))
	if err != nil {
		t.Fatal(err)
	}
	seg, err := mp4.DecodeFile(bytes.NewReader(rawSeg))
	if err != nil {
		t.Fatal(err)
	}
	fragIV := iv
	for _, s := range seg.Segments {
		for _, f := range s.Fragments {
			if fragIV, err = mp4.EncryptFragment(f, key, fragIV, mustProtectData(t, encInit)); err != nil {
				t.Fatal(err)
			}
		}
	}
	var encSegBuf bytes.Buffer
	if err := seg.Encode(&encSegBuf); err != nil {
		t.Fatal(err)
	}

	media := append(append([]byte{}, encInit...), encSegBuf.Bytes()...)

	var wantBuf bytes.Buffer
	if err := widevine.DecryptMP4(bytes.NewReader(media), key, &wantBuf); err != nil {
		t.Fatalf("whole-file decrypt: %v", err)
	}

	keys := []*widevine.Key{{Type: widevinepb.License_KeyContainer_CONTENT, ID: kid, Key: key}}
	var gotBuf bytes.Buffer
	if err := decryptMP4(encInit, bytes.NewReader(media), keys, &gotBuf); err != nil {
		t.Fatalf("streaming decrypt: %v", err)
	}
	if !bytes.Equal(wantBuf.Bytes(), gotBuf.Bytes()) {
		dumpBoxes(t, "whole", wantBuf.Bytes()); dumpBoxes(t, "stream", gotBuf.Bytes()); t.Fatalf("output differs: whole-file %d bytes, streaming %d bytes", wantBuf.Len(), gotBuf.Len())
	}
}

func mustProtectData(t *testing.T, encInit []byte) *mp4.InitProtectData {
	t.Helper()
	encInitFile, err := mp4.DecodeFile(bytes.NewReader(encInit))
	if err != nil {
		t.Fatal(err)
	}
	ipd, err := mp4.ExtractInitProtectData(encInitFile.Init)
	if err != nil {
		t.Fatal(err)
	}
	return ipd
}

func dumpBoxes(t *testing.T, name string, data []byte) {
	t.Helper()
	t.Logf("%s (%d bytes):", name, len(data))
	r := bytes.NewReader(data)
	var pos uint64
	for pos < uint64(len(data)) {
		box, err := mp4.DecodeBox(pos, r)
		if err != nil {
			t.Logf("  decode error at %d: %v", pos, err)
			return
		}
		t.Logf("  %s size=%d", box.Type(), box.Size())
		pos += box.Size()
	}
}
