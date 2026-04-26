//go:build ignore

// genicon writes a 128x128 black-disk placeholder PNG to dist/knomit.png.
// Invoked from the Makefile's Linux tray target via `go run`. The file is
// //go:build ignore so regular package builds skip it; go run supplies its
// own build context.
package main

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

func main() {
	const size = 128
	const cx, cy = size / 2, size / 2
	const r = 56 // leaves an ~8px margin on each side
	const r2 = r * r

	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy < r2 {
				img.Set(x, y, color.NRGBA{0, 0, 0, 255})
			}
		}
	}

	out := filepath.Join("dist", "knomit.png")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		panic(err)
	}
	f, err := os.Create(out)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
}
