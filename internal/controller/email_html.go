package controller

import (
	"fmt"
	"html"
)

// demoReadyEmailHTML returns a branded HTML email body for the "your demo is
// ready" notification. Design tokens match the meshX website email templates
// (inline styles only — email clients do not support CSS variables).
func demoReadyEmailHTML(firstName, tenantURL string) string {
	name := html.EscapeString(firstName)
	if name == "" {
		name = "there"
	}
	escapedURL := html.EscapeString(tenantURL)

	const (
		brand        = "#FE330A"
		brandDark    = "#DA2C09"
		fg           = "#18181b"
		fgMuted      = "#71717a"
		fgSubtle     = "#a1a1aa"
		bg           = "#f4f4f5"
		surface      = "#f9fafb"
		card         = "#ffffff"
		border       = "#e4e4e7"
		footerBg     = "#fafafa"
		footerBorder = "#f4f4f5"
		headerBg     = "#ffffff"
		headerBorder = "#e4e4e7"
	)

	// Dark wordmark (for light header background)
	logo := `<svg width="109" height="20" viewBox="0 0 109 20" fill="none" xmlns="http://www.w3.org/2000/svg" style="display:inline-block;">` +
		`<path fill-rule="evenodd" clip-rule="evenodd" d="M98.0711 10.5582L91.4826 4.11929L93.6194 2.01646L100.23 8.45539L106.841 2.01646L109 4.11929L102.389 10.5582L109 16.976L106.841 19.0788L100.23 12.6399L93.6194 19.0788L91.4826 16.976L98.0711 10.5582ZM32.0292 12.4663C32.4521 14.7644 34.6781 16.1304 38.3063 16.1304C40.866 16.1304 42.5798 15.3499 43.5814 13.9623H47.6993C46.4751 16.8675 43.2032 18.5584 38.3063 18.5584C32.1628 18.5584 28.4011 15.8269 28.4011 11.4691C28.4011 7.11129 31.6286 4.53131 38.3063 4.53131C44.4497 4.53131 47.8551 7.24112 47.8551 11.6423V12.4663H32.0292ZM44.1379 10.0382C43.7372 7.45826 40.2205 6.95936 38.3063 6.95936C34.7671 6.95936 32.8082 8.087 32.1627 10.0382H44.1379ZM74.8774 9.12741V18.1464H71.3606V0H74.8774V6.33054C76.8362 4.9213 79.5741 4.5745 82.0448 4.5745C85.161 4.5745 89.2119 5.87542 89.2119 9.43102V18.1464H85.6951V9.88622C85.6951 7.52305 83.0241 7.04616 80.9986 7.04616C78.7727 7.04616 76.0126 7.73985 74.8774 9.12741ZM3.64926 8.58539V18.1464H0.132411V4.89971H3.64926V6.30894C5.25189 5.05147 7.74492 4.5745 9.25852 4.5745C10.9947 4.5745 12.9312 5.16013 13.9996 6.89457C15.8471 5.03005 18.8742 4.5745 20.3655 4.5745C22.9253 4.5745 26.0639 5.85357 26.0639 10.1246V18.1464H22.5468V10.1246C22.5468 7.95657 20.7441 7.24137 19.275 7.24137C17.8504 7.24137 15.2906 8.32548 14.7341 9.06262C14.7786 9.38783 14.8008 9.75606 14.8008 10.1246V18.1464H11.284V10.1246C11.284 7.95657 9.61466 7.24137 8.14558 7.24137C6.9436 7.24137 4.2725 8.21682 3.64926 8.58539ZM53.7092 13.9623C53.9985 14.7645 54.8444 16.1088 59.6523 16.1088C64.616 16.1088 65.3729 15.1548 65.3729 14.3959C65.3729 13.984 65.0612 12.9867 62.8131 12.7699C61.6334 12.6615 59.9639 12.5312 57.4932 12.4011C50.1923 12.0108 49.9697 9.86471 49.9697 8.75901C49.9697 7.6533 50.0811 4.53131 59.363 4.53131C65.9071 4.53131 68.3332 6.80776 68.5113 8.75901H64.7273C64.5715 7.04625 60.6986 6.93776 59.3408 6.93776C57.983 6.93776 53.6648 7.08935 53.6648 8.60699C53.6648 9.43085 54.8444 9.75606 57.5823 9.90782C58.9401 9.97286 61.5221 10.1466 62.8577 10.255C66.842 10.6019 69.179 11.6423 69.179 14.0055C69.179 15.9784 68.4001 18.4936 59.5856 18.5152C52.3292 18.5369 50.4596 16.4339 49.7695 13.9623H53.7092Z" fill="#18181b"/>` +
		`<path d="M25.3807 0.588096H0.000492234V2.71366H25.3807V0.588096Z" fill="` + brandDark + `"/>` +
		`</svg>`

	stepFmt := `<tr>` +
		`<td style="width:28px;vertical-align:top;padding:%s;">` +
		`<span style="display:inline-block;width:22px;height:22px;background:` + brand + `;border-radius:50%%;text-align:center;line-height:22px;font-size:11px;font-weight:700;color:#fff;">%d</span>` +
		`</td>` +
		`<td style="padding:%s 0 0 10px;vertical-align:top;">` +
		`<p style="margin:0;font-size:14px;font-weight:600;color:` + fg + `;">%s</p>` +
		`<p style="margin:4px 0 0;font-size:13px;color:` + fgMuted + `;line-height:1.5;">%s</p>` +
		`</td></tr>`

	step := func(n int, title, desc, pad string) string {
		return fmt.Sprintf(stepFmt, pad, n, pad, html.EscapeString(title), desc)
	}

	return `<!DOCTYPE html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1.0"></head>` +
		`<body style="margin:0;padding:0;background:` + bg + `;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;">` +
		`<table width="100%" cellpadding="0" cellspacing="0" style="padding:40px 20px;"><tr><td align="center">` +
		`<table width="520" cellpadding="0" cellspacing="0" style="background:` + card + `;border-radius:12px;overflow:hidden;box-shadow:0 1px 3px rgba(0,0,0,0.1);">` +

		// Header with logo — light background, dark wordmark
		`<tr><td style="padding:24px 40px;text-align:left;background:` + headerBg + `;border-bottom:1px solid ` + headerBorder + `;">` + logo + `</td></tr>` +

		// Hero text
		`<tr><td style="padding:40px 40px 32px;text-align:center;">` +
		`<h1 style="margin:0 0 16px;font-size:24px;font-weight:700;color:` + fg + `;">Hi, ` + name + `. Your demo is ready.</h1>` +
		`<p style="margin:0;font-size:15px;color:` + fgMuted + `;line-height:1.6;max-width:400px;margin-inline:auto;">Your Foundation demo environment is live. Click below to explore the platform.</p>` +
		`</td></tr>` +

		// CTA button
		`<tr><td style="padding:0 40px 32px;text-align:center;">` +
		`<a href="` + escapedURL + `" style="display:inline-block;padding:14px 36px;background:` + brand + `;color:#fff;font-size:15px;font-weight:600;text-decoration:none;border-radius:8px;letter-spacing:.01em;">Open your demo &rarr;</a>` +
		`<p style="margin:12px 0 0;font-size:11px;color:` + fgSubtle + `;">Or copy this link: <a href="` + escapedURL + `" style="color:` + fgMuted + `;word-break:break-all;">` + escapedURL + `</a></p>` +
		`</td></tr>` +

		// Steps box
		`<tr><td style="padding:0 40px 32px;">` +
		`<table width="100%" cellpadding="0" cellspacing="0" style="background:` + surface + `;border:1px solid ` + border + `;border-radius:10px;">` +
		`<tr><td style="padding:20px 24px 14px;"><p style="margin:0;font-size:11px;font-weight:700;color:` + fg + `;text-transform:uppercase;letter-spacing:0.06em;">Getting started</p></td></tr>` +
		`<tr><td style="padding:0 24px 20px;"><table width="100%" cellpadding="0" cellspacing="0">` +
		step(1, "Sign in with SSO", `Click <strong>Continue with SSO</strong> and use your work email &mdash; no new password needed.`, "0 0 12px") +
		step(2, "Explore Foundation", "Browse your data catalog, run pipelines, and connect sources in a live environment.", "0 0 12px") +
		step(3, "This environment expires in 72 hours", "Need more time or want to discuss next steps? Reply to this email.", "0 0 0") +
		`</table></td></tr>` +
		`</table>` +
		`</td></tr>` +

		// Reply note
		`<tr><td style="padding:0 40px 32px;text-align:center;">` +
		`<p style="margin:0;font-size:12px;color:` + fgSubtle + `;line-height:1.5;">Questions? Reply to this email or reach us at <a href="mailto:hello@meshx.io" style="color:` + fgMuted + `;text-decoration:underline;">hello@meshx.io</a></p>` +
		`</td></tr>` +

		// Footer
		`<tr><td style="padding:20px 40px;background:` + footerBg + `;border-top:1px solid ` + footerBorder + `;">` +
		`<p style="margin:0;font-size:11px;color:` + fgSubtle + `;text-align:center;">meshX &middot; <a href="https://meshx.io" style="color:` + fgSubtle + `;text-decoration:underline;">meshx.io</a> &middot; <a href="https://docs.meshx.io" style="color:` + fgSubtle + `;text-decoration:underline;">docs</a></p>` +
		`</td></tr>` +

		`</table></td></tr></table>` +
		`</body></html>`
}
