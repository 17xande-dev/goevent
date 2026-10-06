// Package ticket draws a ticket's code as a QR code, for a page and for an
// email. The code itself — what the QR holds — is registrations.Signer's.
package ticket

import (
	"bytes"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"strings"

	"rsc.io/qr"
)

// quiet is the blank border, in modules, the QR specification asks for. A
// scanner finds the code by its edges, and a code drawn flush against a dark
// email background or a table border may not be found at all.
const quiet = 4

// level is medium error correction: a code survives a smudged phone screen or a
// crease in a printout without becoming so dense that a cheap camera struggles.
const level = qr.M

// PNG draws code as a PNG, scale pixels a module, for an email's inline image.
func PNG(code string, scale int) ([]byte, error) {
	c, err := qr.Encode(code, level)
	if err != nil {
		return nil, fmt.Errorf("ticket: encode: %w", err)
	}
	side := (c.Size + 2*quiet) * scale
	img := image.NewGray(image.Rect(0, 0, side, side))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	for y := range c.Size {
		for x := range c.Size {
			if !c.Black(x, y) {
				continue
			}
			for dy := range scale {
				for dx := range scale {
					img.SetGray((x+quiet)*scale+dx, (y+quiet)*scale+dy, color.Gray{})
				}
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("ticket: png: %w", err)
	}
	return buf.Bytes(), nil
}

// SVG draws code as an inline SVG, for a page: sharp at any size and on any
// screen, and drawn from this origin with no image request for the CSP to
// allow. One path, a unit square per dark module, with a white background so
// the code scans on a dark theme too.
func SVG(code string, label string) (template.HTML, error) {
	c, err := qr.Encode(code, level)
	if err != nil {
		return "", fmt.Errorf("ticket: encode: %w", err)
	}
	side := c.Size + 2*quiet
	var path strings.Builder
	for y := range c.Size {
		for x := range c.Size {
			if c.Black(x, y) {
				fmt.Fprintf(&path, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	return template.HTML(fmt.Sprintf(
		`<svg class="qr" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" role="img" aria-label="%s" shape-rendering="crispEdges">`+
			`<rect width="%d" height="%d" fill="#fff"/><path d="%s" fill="#000"/></svg>`,
		side, side, template.HTMLEscapeString(label), side, side, path.String())), nil
}
