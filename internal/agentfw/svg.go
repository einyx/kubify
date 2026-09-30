package agentfw

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
)

// HardenSVGResponse strips dangerous elements and attributes from SVG responses.
// No-op for non-SVG content types.
func HardenSVGResponse(resp *http.Response) error {
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "svg") {
		return nil
	}
	if resp.Body == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if err != nil {
		return err
	}
	safe, err := sanitizeSVG(body)
	if err != nil {
		// If we can't parse it, replace with empty SVG rather than pass dangerous content.
		safe = []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)
	}
	resp.Body = io.NopCloser(bytes.NewReader(safe))
	resp.ContentLength = int64(len(safe))
	return nil
}

var dangerousElements = map[string]bool{
	"script": true, "foreignObject": true, "animate": true,
	"set": true, "animateTransform": true,
}

var dangerousAttrPrefixes = []string{"on"}
var dangerousHrefSchemes = []string{"javascript:", "data:", "vbscript:"}

func sanitizeSVG(src []byte) ([]byte, error) {
	dec := xml.NewDecoder(bytes.NewReader(src))
	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)

	depth := 0 // nesting depth inside a dangerous element
	skip := 0  // skip depth counter

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		switch t := tok.(type) {
		case xml.StartElement:
			local := strings.ToLower(t.Name.Local)
			if skip > 0 || dangerousElements[local] {
				skip++
				_ = depth
				continue
			}
			// Filter dangerous attributes
			var safe []xml.Attr
			for _, a := range t.Attr {
				aName := strings.ToLower(a.Name.Local)
				dangerous := false
				for _, p := range dangerousAttrPrefixes {
					if strings.HasPrefix(aName, p) {
						dangerous = true
						break
					}
				}
				if aName == "href" || (a.Name.Space == "xlink" && aName == "href") {
					val := strings.TrimSpace(strings.ToLower(a.Value))
					for _, s := range dangerousHrefSchemes {
						if strings.HasPrefix(val, s) {
							dangerous = true
							break
						}
					}
				}
				if !dangerous {
					safe = append(safe, a)
				}
			}
			t.Attr = safe
			enc.EncodeToken(t) //nolint:errcheck
			depth++

		case xml.EndElement:
			if skip > 0 {
				skip--
				continue
			}
			enc.EncodeToken(t) //nolint:errcheck
			depth--

		case xml.CharData:
			if skip == 0 {
				enc.EncodeToken(t) //nolint:errcheck
			}

		case xml.Comment, xml.ProcInst:
			// strip comments and processing instructions

		default:
			if skip == 0 {
				enc.EncodeToken(t) //nolint:errcheck
			}
		}
	}
	enc.Flush() //nolint:errcheck
	return buf.Bytes(), nil
}
