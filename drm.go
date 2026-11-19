package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/iyear/gowidevine"
	"github.com/iyear/gowidevine/widevinepb"
)

// widevineSystemID is the Widevine DRM system ID, used to tag PSSH boxes.
var widevineSystemID = mp4.UUID{0xed, 0xef, 0x8b, 0xa9, 0x79, 0xd6, 0x4a, 0xce, 0xa3, 0xc8, 0x27, 0xdc, 0xd5, 0x1d, 0x21, 0xed}

// widevineSchemeSuffix identifies the Widevine ContentProtection element in an
// MPD by its schemeIdUri.
const widevineSchemeSuffix = "edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"

// psshContentProtection is the part of a ContentProtection element that carries
// the PSSH. The element is namespaced (`xmlns:cenc="urn:mpeg:cenc:2013"`), and
// the struct tag deliberately omits the prefix: encoding/xml then matches on the
// local name whatever prefix the manifest happens to use. go-mpd's own
// Descriptor is not used for this because it spells the tag "cenc:pssh", which
// encoding/xml reads as a literal element name and so never matches.
type psshContentProtection struct {
	SchemeIDURI string `xml:"schemeIdUri,attr"`
	PSSH        string `xml:"pssh"`
}

// psshManifest is the subset of the MPD tree needed to find a PSSH. Crunchyroll
// serves two shapes: ContentProtection on the AdaptationSet, or on each
// Representation, so both levels are decoded.
type psshManifest struct {
	AdaptationSets []struct {
		ContentProtections []psshContentProtection `xml:"ContentProtection"`
		Representations    []struct {
			ContentProtections []psshContentProtection `xml:"ContentProtection"`
		} `xml:"Representation"`
	} `xml:"Period>AdaptationSet"`
}

// psshFromProtections returns the Widevine PSSH carried by a list of
// ContentProtection elements, falling back to the first PSSH if none of them is
// explicitly Widevine. Crunchyroll also lists a PlayReady/common system ID
// (9a04f079-9840-4286-ab92-e65be0885f95) alongside Widevine, so the first PSSH
// is not always the right one.
func psshFromProtections(protections []psshContentProtection) *string {
	var fallback *string
	for _, contentProtection := range protections {
		if contentProtection.PSSH == "" {
			continue
		}
		value := contentProtection.PSSH
		if strings.Contains(contentProtection.SchemeIDURI, widevineSchemeSuffix) {
			return &value
		}
		if fallback == nil {
			fallback = &value
		}
	}
	return fallback
}

// getPssh finds the PSSH in the raw MPD manifest, preferring the Widevine one.
func getPssh(body []byte) *string {
	var manifest psshManifest
	if err := xml.Unmarshal(body, &manifest); err != nil {
		return nil
	}
	for _, set := range manifest.AdaptationSets {
		if pssh := psshFromProtections(set.ContentProtections); pssh != nil {
			return pssh
		}
		for _, representation := range set.Representations {
			if pssh := psshFromProtections(representation.ContentProtections); pssh != nil {
				return pssh
			}
		}
	}
	return nil
}

// toWidevinePssh rewrites a PSSH box so its system ID is Widevine's. Crunchyroll
// now serves the Widevine PSSH data inside a box tagged with the PlayReady/common
// system ID, which gowidevine's NewPSSH rejects. The payload itself is left
// untouched; only the system ID field is normalized.
func toWidevinePssh(pssh []byte) ([]byte, error) {
	box, err := mp4.DecodeBox(0, bytes.NewReader(pssh))
	if err != nil {
		return nil, fmt.Errorf("decode PSSH box: %w", err)
	}
	psshBox, ok := box.(*mp4.PsshBox)
	if !ok {
		return nil, fmt.Errorf("box is a %s instead of a PSSH", box.Type())
	}
	if bytes.Equal(psshBox.SystemID, widevineSystemID) {
		return pssh, nil
	}

	psshBox.SystemID = widevineSystemID
	var buf bytes.Buffer
	if err := psshBox.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode PSSH box: %w", err)
	}
	return buf.Bytes(), nil
}

type CrunchyrollWidevineLicenseResponse struct {
	License string `json:"license"`
}

func sendChallenge(contentId, videoToken string, challenge []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, "https://www.crunchyroll.com/license/v1/license/widevine", bytes.NewReader(challenge))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Cr-Content-Id", contentId)
	req.Header.Set("X-Cr-Video-Token", videoToken)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", "https://static.crunchyroll.com")
	req.Header.Set("Referer", "https://static.crunchyroll.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:147.0) Gecko/20100101 Firefox/147.0")
	resp, err := DoRequest(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Parse JSON response
	res, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var result CrunchyrollWidevineLicenseResponse
	if err = json.Unmarshal(res, &result); err != nil {
		return nil, err
	}

	decoded, err := base64.StdEncoding.DecodeString(result.License)
	if err != nil {
		return nil, err
	}

	return decoded, nil
}

// wvdInDir returns the device in dir, preferring device.wvd (the active
// device the web interface manages) over any other .wvd there.
func wvdInDir(dir string) (*widevine.Device, bool, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, false, nil
	}
	var first string
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".wvd") {
			continue
		}
		if file.Name() == "device.wvd" {
			first = file.Name()
			break
		}
		if first == "" {
			first = file.Name()
		}
	}
	if first == "" {
		return nil, false, nil
	}
	wvd, err := os.Open(filepath.Join(dir, first))
	if err != nil {
		return nil, true, err
	}
	device, err := widevine.NewDevice(widevine.FromWVD(io.NopCloser(wvd)))
	return device, true, err
}

func getWidevineDevice() (*widevine.Device, error) {
	// The web interface points CRDL_CDM_DIR at its device pool; a CLI run
	// with no variable keeps looking in the working directory, as before.
	for _, dir := range []string{os.Getenv("CRDL_CDM_DIR"), "."} {
		if dir == "" {
			continue
		}
		if device, found, err := wvdInDir(dir); found || err != nil {
			return device, err
		}
	}

	var clientID []byte
	var privateKey []byte
	files, _ := os.ReadDir(".")
	for _, file := range files {
		if file.Name() == "client_id.bin" {
			f, err := os.Open("client_id.bin")
			if err != nil {
				return nil, err
			}
			defer f.Close()

			clientID, err = io.ReadAll(f)
		} else if file.Name() == "private_key.pem" {
			f, err := os.Open("private_key.pem")
			if err != nil {
				return nil, err
			}
			defer f.Close()

			privateKey, err = io.ReadAll(f)
			break
		}
	}

	if len(clientID) > 0 && len(privateKey) > 0 {
		return widevine.NewDevice(widevine.FromRaw(clientID, privateKey))
	}

	return nil, nil
}

func getLicense(psshData, contentId, videoToken string) ([]*widevine.Key, error) {
	device, err := getWidevineDevice()
	if device == nil {
		return nil, errors.New("no widevine device provided. You either need:\n- a \".wvd\" file,\n- or \"client_id.bin\" and \"private_key.pem\" files.\nI'm not sharing links for obvious reasons, but search \"ready to use cdms\" on Google :)\n")
	} else if err != nil {
		return nil, err
	}
	cdm := widevine.NewCDM(device)
	decodedPssh, err := base64.StdEncoding.DecodeString(psshData)
	if err != nil {
		return nil, err
	}
	normalizedPssh, err := toWidevinePssh(decodedPssh)
	if err != nil {
		return nil, err
	}
	pssh, err := widevine.NewPSSH(normalizedPssh)
	if err != nil {
		return nil, err
	}

	challenge, parseLicense, err := cdm.GetLicenseChallenge(pssh, widevinepb.LicenseType_AUTOMATIC, false)
	if err != nil {
		return nil, err
	}
	resp, err := sendChallenge(contentId, videoToken, challenge)
	if err != nil {
		return nil, err
	}
	keys, err := parseLicense(resp)
	if err != nil {
		return nil, err
	}

	return keys, nil
}

// decryptMP4 decrypts a fragmented MP4 read from media, choosing the content
// key whose ID matches the track's KID. gowidevine's DecryptMP4Auto instead
// uses the first content key in the license, which decrypts to garbage when the
// license carries a separate key per track.
//
// initData is the initialization segment on its own. The KID lives there, so
// the key can be selected without reading the media.
func decryptMP4(initData []byte, media io.Reader, licenseKeys []*widevine.Key, output io.Writer) error {
	init, err := mp4.DecodeFile(bytes.NewReader(initData))
	if err != nil {
		return fmt.Errorf("decode MP4 init segment: %w", err)
	}
	if init.Init == nil {
		return errors.New("MP4 has no initialization segment")
	}

	decryptInfo, err := mp4.DecryptInit(init.Init)
	if err != nil {
		return fmt.Errorf("read MP4 encryption info: %w", err)
	}

	var unmatched []string
	for _, track := range decryptInfo.TrackInfos {
		if track.Sinf == nil || track.Sinf.Schi == nil || track.Sinf.Schi.Tenc == nil {
			continue
		}
		kid := track.Sinf.Schi.Tenc.DefaultKID
		// Match this track's KID with a content key returned by the license.
		for _, key := range licenseKeys {
			if key.Type == widevinepb.License_KeyContainer_CONTENT && bytes.Equal(key.ID, kid) {
				return decryptMP4Streaming(media, decryptInfo, init.Init.Moov, key.Key, output)
			}
		}
		// Keep looking: a later track may still match a key in the license.
		unmatched = append(unmatched, fmt.Sprintf("%x", kid))
	}

	if len(unmatched) > 0 {
		return fmt.Errorf("no license key found for MP4 KID(s) %s", strings.Join(unmatched, ", "))
	}
	return errors.New("MP4 has no encrypted tracks")
}

// decryptMP4Streaming decrypts a fragmented MP4 one fragment at a time.
//
// gowidevine's DecryptMP4 (mp4ff DecodeFile) parses the WHOLE file into a
// File struct, so a 1080p episode needs as much RAM as the encrypted file
// itself (several GB) and the Go heap is not returned to the OS afterwards —
// on a small container that exhausts memory and freezes the whole system.
// Here each moof/mdat pair is decoded, decrypted and written out before the
// next one is read, so peak memory is one fragment (a few MB) regardless of
// the episode length. The loop mirrors mp4ff's DecodeFile: senc parsing per
// traf, boxes before a moof attached to its fragment, trailing boxes flushed
// at EOF.
func decryptMP4Streaming(media io.Reader, decryptInfo mp4.DecryptInfo, moov *mp4.MoovBox, key []byte, output io.Writer) error {
	var pos uint64
	var preBoxes []mp4.Box // boxes since the last mdat that belong to the next fragment
	var moof *mp4.MoofBox
	flush := func(frag *mp4.Fragment) error {
		seg := &mp4.MediaSegment{}
		seg.AddFragment(frag)
		if err := mp4.DecryptSegment(seg, decryptInfo, key); err != nil {
			return err
		}
		return frag.Encode(output)
	}
	for {
		box, err := mp4.DecodeBox(pos, media)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to decode file: %w", err)
		}
		switch box.Type() {
		case "moov":
			moov = box.(*mp4.MoovBox)
			// DecryptInit mutates the moov in place (RemoveEncryption drops the
			// sinf boxes), so the written init must be the box AFTER that
			// call — encoding the raw encrypted moov would leave sinf in the
			// output (the whole-file path's Init.Encode does not have it).
			// The error is ignored: a non-encrypted moov has nothing to strip,
			// and a real problem surfaces as a KID mismatch in decryptMP4.
			_, _ = mp4.DecryptInit(&mp4.InitSegment{Moov: moov})
			for _, b := range preBoxes {
				if err := b.Encode(output); err != nil {
					return fmt.Errorf("failed to write init: %w", err)
				}
			}
			preBoxes = nil
			if err := box.Encode(output); err != nil {
				return fmt.Errorf("failed to write init: %w", err)
			}
		case "moof":
			moof = box.(*mp4.MoofBox)
			for _, traf := range moof.Trafs {
				if ok, parsed := traf.ContainsSencBox(); ok && !parsed {
					var defaultIVSize byte
					if moov != nil && moov.IsEncrypted(traf.Tfhd.TrackID) {
						if sinf := moov.GetSinf(traf.Tfhd.TrackID); sinf != nil && sinf.Schi != nil && sinf.Schi.Tenc != nil {
							defaultIVSize = sinf.Schi.Tenc.DefaultPerSampleIVSize
						}
					}
					if err := traf.ParseReadSenc(defaultIVSize, moof.StartPos); err != nil {
						return err
					}
				}
			}
		case "mdat":
			if moof == nil {
				return errors.New("mdat without preceding moof")
			}
			frag := &mp4.Fragment{Moof: moof, Mdat: box.(*mp4.MdatBox)}
			frag.Children = append(append([]mp4.Box{}, preBoxes...), moof, box)
			if err := flush(frag); err != nil {
				return err
			}
			preBoxes = nil
			moof = nil
		default:
			preBoxes = append(preBoxes, box)
		}
		pos += box.Size()
	}
	if moof != nil {
		return errors.New("truncated file: moof without mdat")
	}
	for _, box := range preBoxes {
		if err := box.Encode(output); err != nil {
			return err
		}
	}
	return nil
}
