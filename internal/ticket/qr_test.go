package ticket

import (
	"bytes"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const code = "3f2504e0-4f89-41d3-9a0c-0305e82c3301.6E7Z_vVAPUNP1B4Nq2BGdQ"

func TestPNG_HasAQuietZoneAndDarkModules(t *testing.T) {
	b, err := PNG(code, 4)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	side := img.Bounds().Dx()
	if side != img.Bounds().Dy() || side%4 != 0 {
		t.Fatalf("image is %v, want a square multiple of the scale", img.Bounds())
	}
	// The border is white all round: without it a scanner on a dark email
	// background may never find the code's edges.
	for i := range quiet * 4 {
		for _, p := range [][2]int{{i, side / 2}, {side - 1 - i, side / 2}, {side / 2, i}, {side / 2, side - 1 - i}} {
			if r, _, _, _ := img.At(p[0], p[1]).RGBA(); r != 0xffff {
				t.Fatalf("quiet zone pixel %v is not white", p)
			}
		}
	}
	// And the finder pattern's corner, just inside the border, is dark.
	if r, _, _, _ := img.At(quiet*4, quiet*4).RGBA(); r != 0 {
		t.Error("the top-left finder pattern is not where it should be")
	}
}

// The test that matters most: a real decoder reads the code back. Drawing the
// right number of squares in the wrong places would pass everything else here
// and fail at the door. Skipped where zbar is not installed.
func TestPNG_DecodesBackToTheCode(t *testing.T) {
	zbar, err := exec.LookPath("zbarimg")
	if err != nil {
		t.Skip("zbarimg not installed")
	}
	b, err := PNG(code, 6)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "ticket.png")
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(zbar, "--quiet", "--raw", file).Output()
	if err != nil {
		t.Fatalf("zbarimg could not read the code: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != code {
		t.Errorf("decoded %q, want %q", got, code)
	}
}

func TestSVG_IsEscapedAndSelfContained(t *testing.T) {
	svg, err := SVG(code, `Ticket for "Ada" <script>`)
	if err != nil {
		t.Fatal(err)
	}
	s := string(svg)
	if !strings.HasPrefix(s, "<svg") || !strings.Contains(s, `<path d="M`) {
		t.Errorf("not an SVG with a path: %.80s", s)
	}
	if strings.Contains(s, "<script>") || strings.Contains(s, `"Ada"`) {
		t.Error("the label was not escaped")
	}
	// Nothing a CSP would refuse: no style attribute, no external reference.
	if strings.Contains(s, "style=") || strings.Contains(s, "href") {
		t.Errorf("the SVG carries a style or a reference: %.200s", s)
	}
}
