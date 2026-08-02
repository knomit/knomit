//go:build desktop

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// badgeColor is the knomit green (fill="#7c9" in web/public/logo.svg), used
// for the update-available dot. The tray glyphs are deliberately monochrome —
// they are the app art with the green diamond recolored away — so the brand
// green is the one hue that is both absent from the base icons and legible on
// a light and a dark menu bar alike.
var badgeColor = color.NRGBA{R: 0x77, G: 0xCC, B: 0x99, A: 0xFF}

// bootBadgeColor is the amber dot shown while the server is still booting. It
// has to be distinguishable from badgeColor at menu-bar size, so it is picked
// far around the hue wheel from the green rather than merely darker, and — like
// the green — it is legible on a light and a dark bar alike.
var bootBadgeColor = color.NRGBA{R: 0xE8, G: 0xA3, B: 0x3D, A: 0xFF}

// Badge geometry, as fractions of the icon's width so the dot holds its
// proportions if the assets are ever re-rendered at another size.
const (
	badgeRadiusFrac = 0.13 // dot radius
	badgeMoatFrac   = 0.18 // radius of the transparent gap punched around it
)

// withUpdateBadge returns src marked with the green "an update is waiting" dot.
func withUpdateBadge(src []byte) []byte { return withBadge(src, badgeColor) }

// withBootBadge returns src marked with the amber "still starting up" dot.
func withBootBadge(src []byte) []byte { return withBadge(src, bootBadgeColor) }

// withBadge returns src with a filled dot in the given color composited into
// its bottom-right corner.
//
// The gap between the dot and the glyph is punched to TRANSPARENT rather than
// filled with a color: the tray icon is drawn straight onto the menu bar, so a
// hole separates the badge on a light bar and a dark one alike, where any
// fixed fill would only work on one of them.
//
// Any failure returns src unchanged. A badge is an embellishment; losing the
// tray icon entirely because a dot could not be drawn would be a far worse
// outcome than an un-badged icon.
func withBadge(src []byte, dot color.NRGBA) []byte {
	img, err := png.Decode(bytes.NewReader(src))
	if err != nil {
		log.Warn().Err(err).Msg("tray: badge not drawn (decode)")
		return src
	}

	b := img.Bounds()
	out := image.NewNRGBA(b)
	draw.Draw(out, b, img, b.Min, draw.Src)

	r := float64(b.Dx()) * badgeRadiusFrac
	moat := float64(b.Dx()) * badgeMoatFrac
	// Inset by the moat, not the dot, so the transparent gap stays inside the
	// canvas — a moat clipped by the edge reads as a bite out of the icon.
	cx := float64(b.Max.X) - moat
	cy := float64(b.Max.Y) - moat

	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			if a := coverage(moat, d); a > 0 {
				p := out.NRGBAAt(x, y)
				p.A = uint8(math.Round(float64(p.A) * (1 - a)))
				out.SetNRGBA(x, y, p)
			}
			if a := coverage(r, d); a > 0 {
				out.SetNRGBA(x, y, over(out.NRGBAAt(x, y), dot, a))
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		log.Warn().Err(err).Msg("tray: badge not drawn (encode)")
		return src
	}
	return buf.Bytes()
}

// coverage returns how much of a pixel at distance d falls inside a disc of
// the given radius, as a 0..1 alpha. The half-pixel ramp is a cheap
// antialias — without it the dot is visibly stair-stepped at menu-bar size.
func coverage(radius, d float64) float64 {
	switch a := radius + 0.5 - d; {
	case a <= 0:
		return 0
	case a >= 1:
		return 1
	default:
		return a
	}
}

// over composites src onto dst with the given extra alpha, in
// non-premultiplied space.
func over(dst, src color.NRGBA, alpha float64) color.NRGBA {
	sa := alpha * float64(src.A) / 255
	da := float64(dst.A) / 255
	outA := sa + da*(1-sa)
	if outA == 0 {
		return color.NRGBA{}
	}
	ch := func(s, d uint8) uint8 {
		return uint8(math.Round((float64(s)*sa + float64(d)*da*(1-sa)) / outA))
	}
	return color.NRGBA{
		R: ch(src.R, dst.R),
		G: ch(src.G, dst.G),
		B: ch(src.B, dst.B),
		A: uint8(math.Round(outA * 255)),
	}
}

// trayIconState owns the tray icon: the theme-appropriate base art, plus at
// most one corner badge — amber while the server is still booting, green once
// an update check has found a release.
//
// All three inputs converge on one apply() and apply() is the ONLY caller of
// setIcon, so the theme swap and the badges cannot race each other into a
// half-correct icon — badged but light-glyph-on-a-light-bar, or theme-correct
// but silently un-badged.
//
// base and setIcon are injected rather than read from the App/SystemTray so
// the state machine is testable without a running Wails application.
type trayIconState struct {
	base    func() []byte
	setIcon func([]byte)

	mu              sync.Mutex
	booting         bool
	updateAvailable bool
}

// newTrayIconState wires the tray icon to the app's appearance and returns the
// handle the boot sequence and the updater use to badge it.
//
// It starts in the booting state: the server comes up on a goroutine while the
// tray is already on the menu bar (see run), so the amber dot is the honest
// initial state and setBooting(false) clears it.
//
// The icon IS set synchronously, before application.Run. macOS builds the
// status item from whatever icon the SystemTray already holds and installs
// nothing if that is nil, so skipping this leaves an empty slot on the menu bar
// until the first apply. Pre-Run the set is just a field write — SetIcon only
// reaches the main thread once the tray has an impl — so it is safe here.
//
// The appearance behind that first pick can be wrong: on macOS it is read from
// the AppleInterfaceStyle user default, which reports light even in dark mode
// until the app has finished launching. watchTrayAppearance re-applies on
// events.Common.ApplicationStarted, which corrects it — so the cost of guessing
// early is at worst a briefly mistoned glyph, against a menu bar that would
// otherwise show nothing at all.
func newTrayIconState(app *application.App, tray *application.SystemTray) *trayIconState {
	t := &trayIconState{
		base:    func() []byte { return baseTrayIcon(app) },
		setIcon: func(b []byte) { tray.SetIcon(b) },
		booting: true,
	}
	watchTrayAppearance(app, t.apply)
	t.apply()
	return t
}

func (t *trayIconState) apply() {
	t.mu.Lock()
	booting, update := t.booting, t.updateAvailable
	t.mu.Unlock()

	icon := t.base()
	// One dot at a time, and boot wins: two dots differing only by hue are
	// unreadable at menu-bar size, and "not ready yet" is the more urgent of
	// the two facts. The green dot is not lost — setBooting re-applies, so it
	// appears the moment boot finishes.
	switch {
	case booting:
		icon = withBootBadge(icon)
	case update:
		icon = withUpdateBadge(icon)
	}
	// SystemTray.SetIcon marshals to the main thread itself (InvokeSync), so
	// this is safe to reach from the boot and update poll goroutines.
	t.setIcon(icon)
}

// setBooting badges (or un-badges) the tray icon with the amber "starting up"
// dot. Like setUpdateAvailable, a redundant call is dropped.
func (t *trayIconState) setBooting(on bool) {
	t.set(&t.booting, on)
}

// setUpdateAvailable badges (or un-badges) the tray icon. Redundant calls are
// dropped: the poll re-announces the same release every interval, and
// re-encoding the PNG to set an icon that is already correct is pure waste.
func (t *trayIconState) setUpdateAvailable(on bool) {
	t.set(&t.updateAvailable, on)
}

// set writes one of the badge flags under the lock and re-applies only when it
// actually changed. field must point at a field of t, so the caller's write is
// covered by t.mu.
func (t *trayIconState) set(field *bool, on bool) {
	t.mu.Lock()
	changed := *field != on
	*field = on
	t.mu.Unlock()

	if changed {
		t.apply()
	}
}
