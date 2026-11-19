package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Widevine devices (.wvd) live in the data directory, not next to the binary:
// the installed service runs with a read-only system filesystem outside its
// data and download directories, so it cannot write a device.wvd into /opt.
// Layout under <data-dir>/cdms/:
//
//	device.wvd   the active device, the one downloads actually use
//	pool/        every device handed over, deduplicated by content
//
// The download child is pointed at this directory with CRDL_CDM_DIR (see
// webConfig.childEnv) and reads device.wvd from it.
const (
	cdmDirName  = "cdms"
	cdmPoolName = "pool"
	cdmActive   = "device.wvd"

	// Uploads are small (a .wvd is a few KB); these bounds keep a hostile or
	// mistaken ZIP from filling the disk.
	maxCDMUpload   = 64 << 20 // whole request
	maxCDMZipBytes = 32 << 20 // uncompressed total of one ZIP
	maxCDMFiles    = 100
)

// cdmDevice is one pooled device as the page sees it.
type cdmDevice struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Current bool   `json:"current"`
}

// cdmInventory is what the page draws: how many devices there are, which one
// is active, and the pool.
type cdmInventory struct {
	Count   int         `json:"count"`
	Current string      `json:"current"` // pooled name of the active device, "" if none
	Devices []cdmDevice `json:"devices"`
}

// cdmStore owns the device pool under a data directory.
type cdmStore struct {
	mu   sync.Mutex
	dir  string
	pool string
}

func newCDMStore(dataDir string) (*cdmStore, error) {
	s := &cdmStore{dir: filepath.Join(dataDir, cdmDirName)}
	s.pool = filepath.Join(s.dir, cdmPoolName)
	if err := os.MkdirAll(s.pool, 0o700); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *cdmStore) activePath() string { return filepath.Join(s.dir, cdmActive) }

// hashOf returns the SHA-256 of a file's contents, or "" if it can't be read.
func hashOf(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// poolLocked lists the pooled devices, oldest name first (os.ReadDir sorts).
// Must be called with s.mu held.
func (s *cdmStore) poolLocked() ([]cdmDevice, error) {
	entries, err := os.ReadDir(s.pool)
	if err != nil {
		return nil, err
	}
	var devices []cdmDevice
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".wvd") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		devices = append(devices, cdmDevice{Name: e.Name(), Size: info.Size()})
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })
	return devices, nil
}

// findByHashLocked returns the pooled name whose contents hash to h, or "".
func (s *cdmStore) findByHashLocked(h string) string {
	devices, _ := s.poolLocked()
	for _, d := range devices {
		if hashOf(filepath.Join(s.pool, d.Name)) == h {
			return d.Name
		}
	}
	return ""
}

// importLocked stores data as a pooled device, deduplicating by content: the
// same device uploaded twice (or inside two ZIPs) stays one entry. It returns
// the pooled name and whether it was new. Must be called with s.mu held.
func (s *cdmStore) importLocked(original string, data []byte) (string, bool, error) {
	if len(data) == 0 {
		return "", false, errors.New("empty file")
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if name := s.findByHashLocked(hash); name != "" {
		return name, false, nil
	}
	base := sanitizeWVDName(original)
	stored := fmt.Sprintf("%s-%s.wvd", base, hash[:12])
	if err := writeFileAtomic(filepath.Join(s.pool, stored), data, 0o600); err != nil {
		return "", false, err
	}
	return stored, true, nil
}

// sanitizeWVDName keeps a readable label for a device from whatever name the
// upload carried, without any path separators or surprises.
func sanitizeWVDName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimSuffix(name, filepath.Ext(name))
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "device"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "device"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// setActiveLocked makes the pooled device name the active one, by copying it
// to device.wvd. Must be called with s.mu held.
func (s *cdmStore) setActiveLocked(name string) error {
	data, err := os.ReadFile(filepath.Join(s.pool, name))
	if err != nil {
		return err
	}
	return writeFileAtomic(s.activePath(), data, 0o600)
}

// ensureActiveLocked reconciles device.wvd with the pool: a device.wvd that
// was placed by hand (or left by an older install) is imported into the pool,
// and if there is no active device yet the first pooled one becomes active.
// Must be called with s.mu held.
func (s *cdmStore) ensureActiveLocked() {
	if _, err := os.Stat(s.activePath()); err == nil {
		if data, err := os.ReadFile(s.activePath()); err == nil {
			_, _, _ = s.importLocked(cdmActive, data)
		}
		return
	}
	devices, err := s.poolLocked()
	if err != nil || len(devices) == 0 {
		return
	}
	_ = s.setActiveLocked(devices[0].Name)
}

// inventory returns the pool with the active device marked.
func (s *cdmStore) inventory() cdmInventory {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureActiveLocked()
	devices, _ := s.poolLocked()
	current := ""
	if h := hashOf(s.activePath()); h != "" {
		current = s.findByHashLocked(h)
	}
	if devices == nil {
		devices = []cdmDevice{}
	}
	for i := range devices {
		devices[i].Current = devices[i].Name == current
	}
	return cdmInventory{Count: len(devices), Current: current, Devices: devices}
}

// importFrom adopts a device file from elsewhere (the device.wvd that older
// installs placed next to the binary) into the pool.
func (s *cdmStore) importFrom(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _, _ = s.importLocked(filepath.Base(path), data)
	s.ensureActiveLocked()
}

// add stores every device in files, returning how many were new and how many
// were already known, plus the resulting inventory.
func (s *cdmStore) add(files []uploadedWVD) (added, duplicated int, inv cdmInventory, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range files {
		_, isNew, ferr := s.importLocked(f.name, f.data)
		if ferr != nil {
			return added, duplicated, s.inventoryLocked(), ferr
		}
		if isNew {
			added++
		} else {
			duplicated++
		}
	}
	s.ensureActiveLocked()
	return added, duplicated, s.inventoryLocked(), nil
}

// rotate makes the next pooled device (in name order, wrapping around) the
// active one. It reports the new active name; with a single device nothing
// changes and rotated is false.
func (s *cdmStore) rotate() (current string, rotated bool, inv cdmInventory, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureActiveLocked()
	devices, err := s.poolLocked()
	if err != nil {
		return "", false, s.inventoryLocked(), err
	}
	if len(devices) == 0 {
		return "", false, s.inventoryLocked(), errors.New("no hay ningún dispositivo WVD disponible; sube uno primero")
	}
	current = s.findByHashLocked(hashOf(s.activePath()))
	next := devices[0].Name
	for i, d := range devices {
		if d.Name == current {
			next = devices[(i+1)%len(devices)].Name
			break
		}
	}
	if next == current {
		return current, false, s.inventoryLocked(), nil
	}
	if err := s.setActiveLocked(next); err != nil {
		return "", false, s.inventoryLocked(), err
	}
	return next, true, s.inventoryLocked(), nil
}

// inventoryLocked is inventory without re-taking the lock. Must be called
// with s.mu held.
func (s *cdmStore) inventoryLocked() cdmInventory {
	devices, _ := s.poolLocked()
	current := ""
	if h := hashOf(s.activePath()); h != "" {
		current = s.findByHashLocked(h)
	}
	if devices == nil {
		devices = []cdmDevice{}
	}
	for i := range devices {
		devices[i].Current = devices[i].Name == current
	}
	return cdmInventory{Count: len(devices), Current: current, Devices: devices}
}

// uploadedWVD is one device handed over, whatever container it arrived in.
type uploadedWVD struct {
	name string
	data []byte
}

// extractWVDs turns an uploaded file into the devices inside it: a single
// .wvd, or every .wvd in a ZIP (nested folders included, anything else in
// the ZIP ignored). Names are reduced to their base name, so a ZIP can't
// write outside the pool.
func extractWVDs(filename string, data []byte) ([]uploadedWVD, error) {
	if strings.HasSuffix(strings.ToLower(filename), ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, fmt.Errorf("el ZIP no se pudo leer: %w", err)
		}
		var files []uploadedWVD
		var total int64
		for _, f := range zr.File {
			if f.FileInfo().IsDir() || !strings.HasSuffix(strings.ToLower(f.Name), ".wvd") {
				continue
			}
			if f.FileInfo().Mode()&os.ModeSymlink != 0 {
				continue
			}
			total += int64(f.UncompressedSize64)
			if total > maxCDMZipBytes {
				return nil, errors.New("el ZIP descomprime a más de 32 MB")
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			body, err := io.ReadAll(io.LimitReader(rc, maxCDMZipBytes))
			rc.Close()
			if err != nil {
				return nil, err
			}
			// Only the base name is kept, so "../../x.wvd" can't escape.
			base := f.Name[strings.LastIndex(f.Name, "/")+1:]
			files = append(files, uploadedWVD{name: base, data: body})
			if len(files) > maxCDMFiles {
				return nil, fmt.Errorf("demasiados dispositivos en el ZIP (máximo %d)", maxCDMFiles)
			}
		}
		if len(files) == 0 {
			return nil, errors.New("el ZIP no contiene ningún archivo .wvd")
		}
		return files, nil
	}

	if !strings.HasSuffix(strings.ToLower(filename), ".wvd") {
		return nil, errors.New("solo se aceptan archivos .wvd o ZIP con .wvd dentro")
	}
	return []uploadedWVD{{name: filename, data: data}}, nil
}

// --- HTTP handlers ---------------------------------------------------------

// cdmStoreFor returns the device store, creating it on first use so a server
// built without one (older tests) still serves the endpoints.
func (w *webServer) cdmStoreFor() *cdmStore {
	w.cdmOnce.Do(func() {
		if w.cdms == nil {
			w.cdms, _ = newCDMStore(w.dataDir)
		}
	})
	return w.cdms
}

func (w *webServer) handleGetCDMs(rw http.ResponseWriter, r *http.Request) {
	writeJSON(rw, http.StatusOK, w.cdmStoreFor().inventory())
}

// handleUploadCDM accepts one .wvd or a ZIP of them and pools them. It is
// refused while a download is running, so a device can't change mid-license.
func (w *webServer) handleUploadCDM(rw http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if w.store.hasRunningJob() {
		writeError(rw, http.StatusConflict, "hay una descarga en marcha; sube el dispositivo cuando termine")
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, maxCDMUpload)
	if err := r.ParseMultipartForm(maxCDMUpload); err != nil {
		writeError(rw, http.StatusBadRequest, "no se pudo leer el archivo subido: %v", err)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(rw, http.StatusBadRequest, "falta el archivo (campo \"file\")")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxCDMUpload))
	if err != nil {
		writeError(rw, http.StatusBadRequest, "no se pudo leer el archivo subido: %v", err)
		return
	}
	files, err := extractWVDs(header.Filename, data)
	if err != nil {
		writeError(rw, http.StatusBadRequest, "%v", err)
		return
	}
	added, duplicated, inv, err := w.cdmStoreFor().add(files)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, "no se pudo guardar el dispositivo: %v", err)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{
		"added": added, "duplicated": duplicated,
		"count": inv.Count, "current": inv.Current, "devices": inv.Devices,
	})
}

// handleRotateCDM makes the next pooled device the active one.
func (w *webServer) handleRotateCDM(rw http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if w.store.hasRunningJob() {
		writeError(rw, http.StatusConflict, "hay una descarga en marcha; rota el dispositivo cuando termine")
		return
	}
	current, rotated, inv, err := w.cdmStoreFor().rotate()
	if err != nil {
		writeError(rw, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{
		"rotated": rotated, "current": current,
		"count": inv.Count, "devices": inv.Devices,
	})
}
