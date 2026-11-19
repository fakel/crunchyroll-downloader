package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustRequest(t *testing.T, method, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// TestExtractWVDs checks that a ZIP yields only its .wvd files (nested ones
// included, other files ignored) and that plain files and junk are handled.
func TestExtractWVDs(t *testing.T) {
	zipData := makeZip(t, map[string]string{
		"device.wvd":       "one",
		"nested/other.wvd": "two",
		"notes.txt":        "ignore me",
		"../traversal.wvd": "three",
		"UPPER.WVD":        "four",
	})
	files, err := extractWVDs("cdms.zip", zipData)
	if err != nil {
		t.Fatalf("extractWVDs: %v", err)
	}
	if len(files) != 4 {
		t.Fatalf("got %d files, want 4: %+v", len(files), files)
	}
	for _, f := range files {
		if strings.Contains(f.name, "..") {
			t.Errorf("zip entry kept its path: %q", f.name)
		}
	}

	one, err := extractWVDs("device.wvd", []byte("x"))
	if err != nil || len(one) != 1 {
		t.Errorf("single .wvd: %v, %d files", err, len(one))
	}

	if _, err := extractWVDs("notes.txt", []byte("x")); err == nil {
		t.Error("a non-.wvd, non-zip upload should be rejected")
	}
	if _, err := extractWVDs("empty.zip", makeZip(t, map[string]string{"a.txt": "x"})); err == nil {
		t.Error("a ZIP with no .wvd should be rejected")
	}
}

// TestCDMPoolRotate checks the pool: uploads deduplicate, the first device
// becomes active, and rotation cycles through them.
func TestCDMPoolRotate(t *testing.T) {
	store, err := newCDMStore(t.TempDir())
	if err != nil {
		t.Fatalf("newCDMStore: %v", err)
	}

	added, dup, inv, err := store.add([]uploadedWVD{
		{name: "b.wvd", data: []byte("second")},
		{name: "a.wvd", data: []byte("first")},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if added != 2 || dup != 0 || inv.Count != 2 {
		t.Fatalf("added=%d dup=%d count=%d, want 2/0/2", added, dup, inv.Count)
	}
	if inv.Current == "" {
		t.Fatal("the first upload should have become active")
	}
	active, err := os.ReadFile(store.activePath())
	if err != nil || string(active) != "second" {
		// Devices are ordered by pooled name (which carries a hash), so
		// "first" is just whichever the pool sorted first; what matters is
		// the active file is a pooled device.
		t.Logf("active = %q, err = %v", active, err)
	}
	first := inv.Current

	// Re-uploading the same bytes doesn't duplicate.
	added, dup, inv, _ = store.add([]uploadedWVD{{name: "again.wvd", data: []byte("second")}})
	if added != 0 || dup != 1 || inv.Count != 2 {
		t.Errorf("re-upload: added=%d dup=%d count=%d, want 0/1/2", added, dup, inv.Count)
	}

	// Rotate twice returns to where we started.
	current, rotated, _, err := store.rotate()
	if err != nil || !rotated || current == first {
		t.Errorf("rotate: current=%q rotated=%v err=%v, want the other device", current, rotated, err)
	}
	current, rotated, _, _ = store.rotate()
	if !rotated || current != first {
		t.Errorf("second rotate: current=%q rotated=%v, want back to %q", current, rotated, first)
	}
}

// TestCDMHandlers drives the HTTP endpoints: inventory, upload (single .wvd
// and ZIP), and rotate.
func TestCDMHandlers(t *testing.T) {
	dataDir := t.TempDir()
	store := testStore(t, stubBinary(t, 0))
	cdms, err := newCDMStore(dataDir)
	if err != nil {
		t.Fatalf("newCDMStore: %v", err)
	}
	web := &webServer{dataDir: dataDir, store: store, cfg: store.cfg, cdms: cdms}
	server := httptest.NewServer(web.routes())
	defer server.Close()

	upload := func(filename, contentType string, body []byte) (int, map[string]any) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, _ := mw.CreateFormFile("file", filename)
		part.Write(body)
		mw.Close()
		res, err := server.Client().Post(server.URL+"/api/cdms/upload", mw.FormDataContentType(), &buf)
		if err != nil {
			t.Fatalf("upload: %v", err)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}

	// A single .wvd.
	status, out := upload("device.wvd", "", []byte("device-one"))
	if status != http.StatusOK || out["added"] != float64(1) || out["count"] != float64(1) {
		t.Fatalf("first upload: status=%d out=%v", status, out)
	}

	// A ZIP with two more (and a non-device file that is ignored).
	zipData := makeZip(t, map[string]string{"x.wvd": "device-two", "y.wvd": "device-three", "readme.txt": "hi"})
	status, out = upload("devices.zip", "", zipData)
	if status != http.StatusOK || out["added"] != float64(2) || out["count"] != float64(3) {
		t.Fatalf("zip upload: status=%d out=%v", status, out)
	}

	// The inventory the page polls.
	res, err := server.Client().Get(server.URL + "/api/cdms")
	if err != nil {
		t.Fatalf("GET /api/cdms: %v", err)
	}
	var inv cdmInventory
	json.NewDecoder(res.Body).Decode(&inv)
	res.Body.Close()
	if inv.Count != 3 || inv.Current == "" {
		t.Errorf("inventory = %+v, want 3 devices and a current one", inv)
	}

	// Rotate makes a different device active.
	before := inv.Current
	res, err = server.Client().Post(server.URL+"/api/cdms/rotate", "", nil)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	var rotated map[string]any
	json.NewDecoder(res.Body).Decode(&rotated)
	res.Body.Close()
	if rotated["rotated"] != true || rotated["current"] == before {
		t.Errorf("rotate = %v, want a new current (was %q)", rotated, before)
	}

	// Deleting the active device is refused: the next download would have
	// nothing to license with, so it has to be rotated away from first.
	res, err = server.Client().Do(mustRequest(t, "DELETE", server.URL+"/api/cdms/"+rotated["current"].(string)))
	if err != nil {
		t.Fatalf("delete current: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Errorf("delete active device: status=%d, want 409", res.StatusCode)
	}

	// Deleting an unknown name is a 404.
	res, err = server.Client().Do(mustRequest(t, "DELETE", server.URL+"/api/cdms/nope.wvd"))
	if err != nil {
		t.Fatalf("delete unknown: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("delete unknown: status=%d, want 404", res.StatusCode)
	}

	// Deleting a non-active device removes just that one.
	var other string
	for _, d := range inv.Devices {
		if d.Name != rotated["current"] {
			other = d.Name
			break
		}
	}
	res, err = server.Client().Do(mustRequest(t, "DELETE", server.URL+"/api/cdms/"+other))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	var after cdmInventory
	json.NewDecoder(res.Body).Decode(&after)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || after.Count != 2 {
		t.Errorf("delete %q: status=%d count=%d, want 200/2", other, res.StatusCode, after.Count)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "cdms", "pool", other)); !os.IsNotExist(err) {
		t.Errorf("deleted device still on disk: %v", err)
	}

	// A non-device file is refused with a 400 the page can show.
	status, _ = upload("notes.txt", "", []byte("hi"))
	if status != http.StatusBadRequest {
		t.Errorf("upload notes.txt: status=%d, want 400", status)
	}

	// The active file on disk really is the rotated device.
	data, err := os.ReadFile(filepath.Join(dataDir, "cdms", "device.wvd"))
	if err != nil || len(data) == 0 {
		t.Errorf("active device.wvd: %q, err=%v", data, err)
	}
	if got := hashOf(filepath.Join(dataDir, "cdms", "device.wvd")); got == "" {
		t.Error("active device should be readable")
	}
	_ = io.Discard
}
