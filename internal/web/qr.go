package web

import (
	"fmt"
	"html/template"
	"strings"

	"rsc.io/qr"
)

// qrSVG renders text as an inline SVG QR code.
//
// The tester has to open the install page on the phone, and the realistic alternative is
// copying a long ts.net URL out of a chat message (DESIGN §9). Inline SVG keeps the page
// self-contained: no external assets, no second request.
func qrSVG(text string) (template.HTML, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	size := code.Size

	// One path of rectangles beats size² <rect> elements, and scales crisply.
	var d strings.Builder
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if code.Black(x, y) {
				fmt.Fprintf(&d, "M%d %dh1v1h-1z", x, y)
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b,
		`<svg class="qr" xmlns="http://www.w3.org/2000/svg" viewBox="-1 -1 %d %d" shape-rendering="crispEdges" role="img" aria-label="QR code for this page">`,
		size+2, size+2)
	fmt.Fprintf(&b, `<rect x="-1" y="-1" width="%d" height="%d" fill="#fff"/>`, size+2, size+2)
	fmt.Fprintf(&b, `<path fill="#000" d="%s"/></svg>`, d.String())
	return template.HTML(b.String()), nil
}
