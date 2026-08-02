//go:build desktop

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"sync"
	"testing"
)

// testIcon is a fully opaque square standing in for a tray asset. Synthesised
// rather than read from the embedded PNGs so the badge tests run identically
// under either platform's trayicon_*.go.
func testIcon(t *testing.T) []byte {
	t.Helper()
	return testIconColor(t, color.NRGBA{R: 0x1d, G: 0x1d, B: 0x1f, A: 0xff})
}

// testIconColor builds a flat icon in the given color, so two visually
// DIFFERENT bases can stand in for the light-bar and dark-bar art. Appending
// bytes to one PNG would not do: png.Decode ignores trailing data, so both
// would badge to the identical image and the theme test would pass vacuously.
func testIconColor(t *testing.T, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := range 64 {
		for x := range 64 {
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// samePixel compares two colors by value. The badged output is always NRGBA
// while a decoded source may be any concrete type, so == on color.Color
// compares the dynamic type too and reports a false difference.
func samePixel(a, b color.Color) bool {
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}

func decode(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return img
}

func TestWithUpdateBadgeMarksTheCornerAndLeavesTheGlyph(t *testing.T) {
	src := testIcon(t)
	got := withUpdateBadge(src)

	if bytes.Equal(got, src) {
		t.Fatal("withUpdateBadge returned the input unchanged; the badge is invisible")
	}
	out := decode(t, got)
	if out.Bounds() != decode(t, src).Bounds() {
		t.Fatalf("bounds = %v, want the source's %v — the tray would scale it", out.Bounds(), decode(t, src).Bounds())
	}

	// The badge sits at the bottom-right, centred one moat-radius in.
	moat := badgeOffset(badgeMoatFrac)
	cx, cy := 64-moat, 64-moat
	r, g, b, a := out.At(cx, cy).RGBA()
	if a == 0 {
		t.Fatal("badge centre is transparent; nothing was drawn")
	}
	if got, want := (color.NRGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}), badgeColor; got != want {
		t.Errorf("badge centre = %+v, want the brand green %+v", got, want)
	}

	// The opposite corner — where the glyph lives — must be untouched. A badge
	// that redraws the icon is a badge that can silently blank it.
	if !samePixel(out.At(2, 2), decode(t, src).At(2, 2)) {
		t.Errorf("top-left pixel changed to %v; the badge must only touch its corner", out.At(2, 2))
	}
}

// The gap around the dot is punched to TRANSPARENT rather than filled, because
// the tray icon is drawn straight onto the menu bar: a hole reads on a light
// bar and a dark one alike, where any fixed fill would only work on one.
func TestWithUpdateBadgeSeparatesTheDotWithATransparentGap(t *testing.T) {
	out := decode(t, withUpdateBadge(testIcon(t)))

	// Midway between the dot's edge and the moat's — inside the gap.
	gap := badgeOffset((badgeRadiusFrac + badgeMoatFrac) / 2)
	moat := badgeOffset(badgeMoatFrac)
	cx, cy := 64-moat, 64-moat
	if _, _, _, a := out.At(cx+gap, cy).RGBA(); a != 0 {
		t.Errorf("gap pixel alpha = %d, want 0 — the dot would blend into the glyph", a>>8)
	}
}

// A badge is an embellishment. Losing the tray icon entirely because a dot
// could not be drawn would be far worse than an un-badged icon, so any failure
// returns the source untouched.
func TestWithUpdateBadgeFallsBackToTheSourceOnGarbage(t *testing.T) {
	src := []byte("not a png")
	if got := withUpdateBadge(src); !bytes.Equal(got, src) {
		t.Errorf("withUpdateBadge(garbage) = %q, want the input back", got)
	}
}

// fakeTrayIcon captures what trayIconState hands to the tray.
type fakeTrayIcon struct {
	mu   sync.Mutex
	set  [][]byte
	dark bool
	base map[bool][]byte
}

func (f *fakeTrayIcon) state() *trayIconState {
	return &trayIconState{
		base: func() []byte {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.base[f.dark]
		},
		setIcon: func(b []byte) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.set = append(f.set, b)
		},
	}
}

func (f *fakeTrayIcon) last() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.set) == 0 {
		return nil
	}
	return f.set[len(f.set)-1]
}

func (f *fakeTrayIcon) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.set)
}

func TestTrayIconStateBadgesOnlyOnceAnUpdateIsAvailable(t *testing.T) {
	icon := testIcon(t)
	f := &fakeTrayIcon{base: map[bool][]byte{false: icon}}
	s := f.state()

	s.apply()
	if got := f.last(); !bytes.Equal(got, icon) {
		t.Error("the un-badged icon was modified before any update was found")
	}

	s.setUpdateAvailable(true)
	if got := f.last(); bytes.Equal(got, icon) {
		t.Error("the icon is still un-badged after an update was announced")
	}
}

// The poll re-announces the same release every interval. Re-encoding a PNG to
// set an icon that is already correct is pure waste, so a redundant call must
// not reach the tray at all.
func TestTrayIconStateIgnoresARedundantAnnouncement(t *testing.T) {
	f := &fakeTrayIcon{base: map[bool][]byte{false: testIcon(t)}}
	s := f.state()

	s.setUpdateAvailable(true)
	after := f.count()
	s.setUpdateAvailable(true)
	s.setUpdateAvailable(true)

	if got := f.count(); got != after {
		t.Errorf("SetIcon called %d times for repeats of one announcement, want %d", got, after)
	}
}

// The badge and the theme are two inputs to ONE icon. A theme change after an
// update was found must re-apply both — dropping the badge here would silently
// un-announce the update the moment the user's menu bar changed appearance.
func TestTrayIconStateKeepsTheBadgeAcrossAThemeChange(t *testing.T) {
	light := testIconColor(t, color.NRGBA{R: 0x1d, G: 0x1d, B: 0x1f, A: 0xff})
	dark := testIconColor(t, color.NRGBA{R: 0xf5, G: 0xf5, B: 0xf7, A: 0xff})
	f := &fakeTrayIcon{base: map[bool][]byte{false: light, true: dark}}
	s := f.state()

	s.setUpdateAvailable(true)
	badgedLight := f.last()

	f.mu.Lock()
	f.dark = true
	f.mu.Unlock()
	s.apply() // what watchTrayAppearance does on ThemeChanged

	badgedDark := f.last()
	if bytes.Equal(badgedDark, badgedLight) {
		t.Error("the theme change did not swap the base art")
	}
	if bytes.Equal(badgedDark, dark) {
		t.Error("the theme change dropped the update badge")
	}
}

// The tray goes up before the server does, so the icon has to say so. A tray
// icon indistinguishable from the ready one would claim knomit is serving when
// it is still opening repos.
func TestTrayIconStateBadgesWhileBooting(t *testing.T) {
	icon := testIcon(t)
	f := &fakeTrayIcon{base: map[bool][]byte{false: icon}}
	s := f.state()
	s.booting = true

	s.apply()
	booting := f.last()
	if bytes.Equal(booting, icon) {
		t.Fatal("the icon is un-badged while booting; nothing marks it as not ready")
	}

	moat := badgeOffset(badgeMoatFrac)
	r, g, b, a := decode(t, booting).At(64-moat, 64-moat).RGBA()
	got := color.NRGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
	if got != bootBadgeColor {
		t.Errorf("boot badge centre = %+v, want the amber %+v", got, bootBadgeColor)
	}

	s.setBooting(false)
	if ready := f.last(); !bytes.Equal(ready, icon) {
		t.Error("the boot badge survived the server coming up")
	}
}

// Two dots differing only by hue are unreadable at menu-bar size, so boot wins
// while it lasts — but the update must not be lost. Clearing the boot state has
// to reveal the green dot, not silently un-announce the release.
func TestTrayIconStateRevealsTheUpdateBadgeAfterBooting(t *testing.T) {
	icon := testIcon(t)
	f := &fakeTrayIcon{base: map[bool][]byte{false: icon}}
	s := f.state()
	s.booting = true

	s.setUpdateAvailable(true)
	moat := badgeOffset(badgeMoatFrac)
	r, g, b, a := decode(t, f.last()).At(64-moat, 64-moat).RGBA()
	got := color.NRGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
	if got != bootBadgeColor {
		t.Errorf("badge centre while booting = %+v, want the amber %+v — boot takes precedence", got, bootBadgeColor)
	}

	s.setBooting(false)
	r, g, b, a = decode(t, f.last()).At(64-moat, 64-moat).RGBA()
	got = color.NRGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
	if got != badgeColor {
		t.Errorf("badge centre once up = %+v, want the brand green %+v — the update was dropped", got, badgeColor)
	}
}

// badgeOffset converts a badge geometry fraction into a pixel offset in the
// 64px test icon. A helper rather than an inline expression because the
// fractions are untyped float constants: int(64 * badgeMoatFrac) is a constant
// conversion the compiler rejects for a non-integral value.
func badgeOffset(frac float64) int {
	size := 64.0
	return int(size * frac)
}
