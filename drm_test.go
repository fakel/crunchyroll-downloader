package main

import (
	"bytes"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

var playReadySystemID = mp4.UUID{0x9a, 0x04, 0xf0, 0x79, 0x98, 0x40, 0x42, 0x86, 0xab, 0x92, 0xe6, 0x5b, 0xe0, 0x88, 0x5f, 0x95}

func buildPssh(t *testing.T, systemID mp4.UUID, data []byte) []byte {
	t.Helper()
	box := &mp4.PsshBox{SystemID: systemID, Data: data}
	var buf bytes.Buffer
	if err := box.Encode(&buf); err != nil {
		t.Fatalf("encode PSSH box: %v", err)
	}
	return buf.Bytes()
}

func TestToWidevinePssh(t *testing.T) {
	payload := []byte{0x08, 0x01, 0x12, 0x10, 0xaa, 0xbb, 0xcc, 0xdd}

	t.Run("widevine system id is unchanged", func(t *testing.T) {
		original := buildPssh(t, widevineSystemID, payload)
		got, err := toWidevinePssh(original)
		if err != nil {
			t.Fatalf("toWidevinePssh returned error: %v", err)
		}
		if !bytes.Equal(got, original) {
			t.Fatal("toWidevinePssh altered an already-widevine PSSH")
		}
	})

	t.Run("playready system id is rewritten", func(t *testing.T) {
		in := buildPssh(t, playReadySystemID, payload)
		got, err := toWidevinePssh(in)
		if err != nil {
			t.Fatalf("toWidevinePssh returned error: %v", err)
		}

		box, err := mp4.DecodeBox(0, bytes.NewReader(got))
		if err != nil {
			t.Fatalf("decode result: %v", err)
		}
		pssh, ok := box.(*mp4.PsshBox)
		if !ok {
			t.Fatalf("result is %T, want *mp4.PsshBox", box)
		}
		if !bytes.Equal(pssh.SystemID, widevineSystemID) {
			t.Fatalf("system id = %s, want widevine", pssh.SystemID)
		}
		if !bytes.Equal(pssh.Data, payload) {
			t.Fatalf("payload changed: got %x, want %x", pssh.Data, payload)
		}
	})
}

// manifest is a minimal MPD carrying the given ContentProtection elements,
// exactly as Crunchyroll writes them: the PSSH lives in a namespaced
// <cenc:pssh> child. That prefix is the whole point of these tests — a struct tag
// spelled "cenc:pssh" never matches it (encoding/xml treats it as a literal name),
// which silently lost every PSSH when go-mpd was upgraded.
func manifest(adaptationProtections, representationProtections string) []byte {
	if representationProtections != "" {
		representationProtections = "<Representation id=\"v0\">" + representationProtections + "</Representation>"
	}
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:cenc="urn:mpeg:cenc:2013" type="static">
  <Period>
    <AdaptationSet contentType="video">` + adaptationProtections + representationProtections + `</AdaptationSet>
  </Period>
</MPD>`)
}

const (
	widevineURI  = "urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"
	playReadyURI = "urn:uuid:9a04f079-9840-4286-ab92-e65be0885f95"

	widevinePssh  = "widevine-pssh"
	playReadyPssh = "playready-pssh"

	playReadyProtection = `<ContentProtection schemeIdUri="` + playReadyURI + `"><cenc:pssh>` + playReadyPssh + `</cenc:pssh></ContentProtection>`
	widevineProtection  = `<ContentProtection schemeIdUri="` + widevineURI + `"><cenc:pssh>` + widevinePssh + `</cenc:pssh></ContentProtection>`
)

func TestGetPsshPrefersWidevine(t *testing.T) {
	tests := []struct {
		name               string
		adaptation, repres string
		want               string
	}{
		{"widevine preferred over playready", playReadyProtection + widevineProtection, "", widevinePssh},
		{"falls back when no widevine scheme", playReadyProtection, "", playReadyPssh},
		{"nil when no pssh", `<ContentProtection schemeIdUri="` + widevineURI + `"/>`, "", ""},
		{"representation level widevine pssh", "", playReadyProtection + widevineProtection, widevinePssh},
		{"representation level fallback", "", playReadyProtection, playReadyPssh},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getPssh(manifest(tt.adaptation, tt.repres))
			if tt.want == "" {
				if got != nil {
					t.Fatalf("getPssh() = %q, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("getPssh() = nil, want %q", tt.want)
			}
			if *got != tt.want {
				t.Fatalf("getPssh() = %q, want %q", *got, tt.want)
			}
		})
	}
}

func TestGetPsshUnprefixedElement(t *testing.T) {
	// Some manifests write <pssh> without the cenc: prefix; both must parse.
	body := manifest("<ContentProtection schemeIdUri=\""+widevineURI+"\"><pssh>"+widevinePssh+"</pssh></ContentProtection>", "")
	if got := getPssh(body); got == nil || *got != widevinePssh {
		t.Fatalf("getPssh() = %v, want the unprefixed widevine PSSH", got)
	}
}

func TestGetPsshMalformedXML(t *testing.T) {
	if got := getPssh([]byte("<MPD><Period>")); got != nil {
		t.Fatalf("getPssh() = %q, want nil for malformed XML", *got)
	}
}
