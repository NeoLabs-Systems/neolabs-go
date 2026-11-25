package servicemail

// Generated from the Node server's service_email.js: static text is copied
// byte for byte so mail renders identically. {{name}} marks a substitution.

const (
	fontSans  = `Inter,-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Arial,sans-serif`
	fontSerif = `Fraunces,'Iowan Old Style',Georgia,'Times New Roman',serif`
	fontMono  = `'IBM Plex Mono',SFMono-Regular,Menlo,Consolas,monospace`
	fontLink  = `https://fonts.googleapis.com/css2?family=Fraunces:opsz,wght@9..144,560&family=IBM+Plex+Mono:wght@500;600&family=Inter:wght@440;600;700&display=swap`

	darkStyles = `
  :root { color-scheme: light dark; supported-color-schemes: light dark; }
  @media (prefers-color-scheme: dark) {
    .na-bg { background:#0e1511 !important; }
    .na-card { background:#171f1a !important; border-color:#263129 !important; }
    .na-ink { color:#ecefe5 !important; }
    .na-ink-2 { color:#aeb7a6 !important; }
    .na-ink-3 { color:#7e8877 !important; }
    .na-gold { color:#eac272 !important; }
    .na-dot { background:#e1b052 !important; }
    .na-line { border-color:#263129 !important; }
    .na-btn { background:#e1b052 !important; color:#0e1511 !important; }
  }`

	detailRowTmpl = `
    <tr>
      <td class="na-line na-ink-3" style="{{border}}padding:12px 12px 12px 0;color:#7e8470;font-family:{{mono}};font-size:11px;font-weight:500;letter-spacing:.12em;text-transform:uppercase;vertical-align:top;white-space:nowrap;">{{label}}</td>
      <td class="na-line na-ink" style="{{border}}padding:12px 0;color:#1c2117;font-size:13.5px;font-weight:600;text-align:right;word-break:break-word;">{{value}}</td>
    </tr>`

	detailsTableTmpl = `
    <table role="presentation" class="na-line" width="100%" cellspacing="0" cellpadding="0" style="margin-top:28px;border-top:1px solid #e6e2d6;border-bottom:1px solid #e6e2d6;">
      {{rows}}
    </table>
  `

	actionTmpl = `
    <table role="presentation" cellspacing="0" cellpadding="0" style="margin-top:28px;">
      <tr>
        <td class="na-btn" style="border-radius:999px;background:#1c2117;">
          <a href="{{actionUrl}}" class="na-btn" style="display:inline-block;padding:13px 22px;border-radius:999px;background:#1c2117;color:#fdfcf8;font-family:{{sans}};font-size:14.5px;font-weight:600;letter-spacing:-0.01em;text-decoration:none;">{{actionLabel}} &rarr;</a>
        </td>
      </tr>
    </table>
    <p class="na-ink-3" style="margin:18px 0 0;color:#7e8470;font-size:12px;line-height:1.6;">If the button does not work, open this link:<br><a href="{{actionUrl}}" class="na-gold" style="color:#8a5f1c;word-break:break-all;">{{actionUrl}}</a></p>
  `

	pageTmpl = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width,initial-scale=1">
    <meta name="color-scheme" content="light dark">
    <meta name="supported-color-schemes" content="light dark">
    <title>{{title}}</title>
    <link href="{{fontLink}}" rel="stylesheet">
    <style>{{darkStyles}}
    </style>
  </head>
  <body class="na-bg" style="margin:0;padding:0;background:#f4f1e8;font-family:{{sans}};color:#1c2117;-webkit-font-smoothing:antialiased;">
    <div style="display:none;max-height:0;overflow:hidden;color:transparent;">{{title}}</div>
    <table role="presentation" class="na-bg" width="100%" cellspacing="0" cellpadding="0" style="background:#f4f1e8;padding:40px 16px;">
      <tr>
        <td align="center">
          <table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="max-width:560px;">
            <tr>
              <td style="padding:0 6px 18px;">
                <span class="na-ink" style="color:#1c2117;font-family:{{sans}};font-size:17px;font-weight:700;letter-spacing:-0.03em;">{{brand}}</span>
              </td>
            </tr>
          </table>
          <table role="presentation" class="na-card" width="100%" cellspacing="0" cellpadding="0" style="max-width:560px;background:#fdfcf8;border:1px solid #e6e2d6;border-radius:26px;box-shadow:0 18px 48px -24px rgba(28,33,23,0.28),0 8px 20px -12px rgba(28,33,23,0.12);">
            <tr>
              <td style="padding:34px 32px 30px;">
                <table role="presentation" cellspacing="0" cellpadding="0">
                  <tr>
                    <td style="padding-right:9px;vertical-align:middle;"><div class="na-dot" style="width:6px;height:6px;border-radius:999px;background:#b07d2b;box-shadow:0 0 0 4px rgba(176,125,43,0.14);"></div></td>
                    <td class="na-gold" style="vertical-align:middle;color:#8a5f1c;font-family:{{mono}};font-size:11px;font-weight:600;letter-spacing:.16em;text-transform:uppercase;">Account</td>
                  </tr>
                </table>
                <h1 class="na-ink" style="margin:18px 0 0;color:#1c2117;font-family:{{serif}};font-size:30px;font-weight:560;line-height:1.1;letter-spacing:-0.015em;">{{title}}</h1>
                <p class="na-ink-2" style="margin:14px 0 0;color:#49503f;font-size:15.5px;line-height:1.65;">{{intro}}</p>
                {{action}}
                {{details}}
              </td>
            </tr>
          </table>
          <table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="max-width:560px;">
            <tr>
              <td style="padding:20px 6px 0;">
                <p class="na-ink-3" style="margin:0;color:#7e8470;font-size:12px;line-height:1.6;">{{footer}}</p>
              </td>
            </tr>
          </table>
        </td>
      </tr>
    </table>
  </body>
</html>`
)
