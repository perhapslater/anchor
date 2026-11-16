package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

// disc returns a PNG of a filled circle on a see-through background.
func disc(t *testing.T, w, h int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	cx, cy, r := float64(w)/2, float64(h)/2, float64(min(w, h))/2
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx, dy := float64(x)+.5-cx, float64(y)+.5-cy
			if dx*dx+dy*dy <= r*r {
				img.SetNRGBA(x, y, c)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// discPNG is a red disc with the given opacity.
func discPNG(t *testing.T, w, h int, alpha uint8) []byte {
	t.Helper()
	return disc(t, w, h, color.NRGBA{200, 30, 30, alpha})
}

func decodeHome(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	return img
}

func rgb8(img image.Image, x, y int) (r, g, b uint8) {
	r16, g16, b16, _ := img.At(x, y).RGBA()
	return uint8(r16 >> 8), uint8(g16 >> 8), uint8(b16 >> 8)
}

func TestHomeIconIsAnOpaqueSquareOnAPlate(t *testing.T) {
	for name, src := range map[string][]byte{
		"square": discPNG(t, 128, 128, 255),
		"wide":   discPNG(t, 300, 100, 255),
		"tall":   discPNG(t, 100, 300, 255),
		"large":  discPNG(t, 1024, 1024, 255),
	} {
		out, ok := homeIcon(src)
		if !ok {
			t.Fatalf("%s: not converted", name)
		}
		img := decodeHome(t, out)
		if b := img.Bounds(); b.Dx() != 512 || b.Dy() != 512 {
			t.Fatalf("%s: %dx%d, want 512x512", name, b.Dx(), b.Dy())
		}
		for y := 0; y < 512; y++ {
			for x := 0; x < 512; x++ {
				if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
					t.Fatalf("%s: pixel %d,%d is see-through, and iOS paints that black", name, x, y)
				}
			}
		}
		if r, g, b := rgb8(img, 2, 2); r != 0xef || g != 0xeb || b != 0xe4 {
			t.Errorf("%s: corner is %d,%d,%d, want the plate", name, r, g, b)
		}
		if r, g, b := rgb8(img, 256, 256); r < 180 || g > 80 || b > 80 {
			t.Errorf("%s: centre is %d,%d,%d, want the red icon", name, r, g, b)
		}
	}
}

// A half see-through icon must look pink on the plate: not solid red, and not
// darkened by the transparent pixels around it.
func TestHomeIconBlendsSoftEdgesIntoThePlate(t *testing.T) {
	out, ok := homeIcon(discPNG(t, 128, 128, 128))
	if !ok {
		t.Fatal("not converted")
	}
	r, g, b := rgb8(decodeHome(t, out), 256, 256)
	if r < 205 || r > 235 || g < 110 || g > 160 || b < 110 || b > 160 {
		t.Fatalf("centre is %d,%d,%d, want about 219,132,129", r, g, b)
	}
}

func TestHomeIconRefusesWhatItCannotUse(t *testing.T) {
	for name, data := range map[string][]byte{
		"not a png": []byte("<svg/>"),
		"empty":     nil,
		"too big":   discPNG(t, maxIconSide+1, 8, 255),
	} {
		if _, ok := homeIcon(data); ok {
			t.Errorf("%s: should be refused", name)
		}
	}
}

func TestFallbackHomeIconIsOpaqueAndSquare(t *testing.T) {
	img := decodeHome(t, fallbackHomeIcon)
	if b := img.Bounds(); b.Dx() != 512 || b.Dy() != 512 {
		t.Fatalf("%dx%d, want 512x512", b.Dx(), b.Dy())
	}
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0xffff {
		t.Fatal("Anchor's own icon must be opaque too")
	}
}

// pngTransport answers every outbound request with a PNG, and counts them.
type pngTransport struct {
	n    atomic.Int32
	body []byte
}

func (p *pngTransport) RoundTrip(*http.Request) (*http.Response, error) {
	p.n.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(p.body))}, nil
}

func homeMux(s *IconServer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /icons/{name}/home.png", s.ServeHome)
	return mux
}

func getHome(mux http.Handler, name string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/icons/"+name+"/home.png", nil))
	return rec
}

func TestHomeIconsOnlyForConfiguredNames(t *testing.T) {
	bundled := fstest.MapFS{"immich.png": {Data: discPNG(t, 128, 128, 255)}}
	allowed := map[string]bool{"immich": true}
	s := NewIconServer(t.TempDir(), fs.FS(bundled), func(n string) bool { return allowed[n] })
	tr := &countingTransport{}
	s.client.Transport = tr
	mux := homeMux(s)

	rec := getHome(mux, "immich")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("configured icon: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if r, _, _ := rgb8(decodeHome(t, rec.Body.Bytes()), 256, 256); r < 180 {
		t.Error("the bundled PNG wasn't used")
	}
	for _, name := range []string{"jellyfin", "zz-random", "a..b"} {
		if code := getHome(mux, name).Code; code != 404 {
			t.Errorf("%s: got %d, want 404", name, code)
		}
	}
	if n := tr.n.Load(); n != 0 {
		t.Fatalf("names not in anchor.yaml caused %d outbound requests", n)
	}
}

func TestHomeIconFallsBackToAnchorsOwn(t *testing.T) {
	bundled := fstest.MapFS{"only-svg.svg": {Data: []byte("<svg/>")}}
	s := NewIconServer(t.TempDir(), fs.FS(bundled), func(string) bool { return true })
	s.client.Transport = &countingTransport{} // every download says 404
	rec := getHome(homeMux(s), "only-svg")
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), fallbackHomeIcon) {
		t.Fatalf("want Anchor's own icon, got %d (%d bytes)", rec.Code, rec.Body.Len())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("a stand-in icon should be cached only briefly, got %q", cc)
	}
}

func TestHomeIconPrefersYourOwnPNG(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "icons"), 0o755); err != nil {
		t.Fatal(err)
	}
	green := disc(t, 64, 64, color.NRGBA{30, 160, 60, 255})
	if err := os.WriteFile(filepath.Join(dir, "icons", "immich.png"), green, 0o644); err != nil {
		t.Fatal(err)
	}
	bundled := fstest.MapFS{"immich.png": {Data: discPNG(t, 64, 64, 255)}}
	s := NewIconServer(dir, fs.FS(bundled), func(string) bool { return true })
	r, g, _ := rgb8(decodeHome(t, getHome(homeMux(s), "immich").Body.Bytes()), 256, 256)
	if g < 120 || r > 100 {
		t.Fatalf("your own PNG should win over the bundled one, centre is %d,%d", r, g)
	}
}

func TestHomeIconIsDownloadedOnceAndCached(t *testing.T) {
	dir := t.TempDir()
	s := NewIconServer(dir, fs.FS(fstest.MapFS{}), func(string) bool { return true })
	tr := &pngTransport{body: discPNG(t, 256, 256, 255)}
	s.client.Transport = tr
	mux := homeMux(s)
	for i := 0; i < 2; i++ {
		if rec := getHome(mux, "seerr"); rec.Code != 200 || bytes.Equal(rec.Body.Bytes(), fallbackHomeIcon) {
			t.Fatalf("request %d: expected the downloaded icon", i+1)
		}
	}
	if n := tr.n.Load(); n != 1 {
		t.Fatalf("downloaded %d times, want once", n)
	}
	if _, err := os.Stat(filepath.Join(dir, ".icon-cache", "seerr.png")); err != nil {
		t.Fatalf("not cached: %v", err)
	}
}

// Home-screen icons need a PNG and the icons Anchor ships are SVGs, so every
// bundled SVG has a PNG copy next to it.
func TestEveryBundledIconHasAPNGCopy(t *testing.T) {
	icons, err := fs.Sub(webFiles, "web/icons")
	if err != nil {
		t.Fatal(err)
	}
	svgs, _ := fs.Glob(icons, "*.svg")
	if len(svgs) == 0 {
		t.Fatal("no bundled icons found")
	}
	for _, svg := range svgs {
		name := strings.TrimSuffix(svg, ".svg")
		data, err := fs.ReadFile(icons, name+".png")
		if err != nil {
			t.Errorf("%s has no PNG copy: %v", name, err)
			continue
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil || cfg.Width < 256 || cfg.Height < 256 {
			t.Errorf("%s.png isn't a usable PNG of at least 256px: %v %dx%d", name, err, cfg.Width, cfg.Height)
		}
	}
}
