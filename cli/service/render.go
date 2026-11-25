package service

import (
	"path/filepath"
	"strings"
)

func escapeXML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

// RenderLaunchAgent is the launchd agent: it starts at login, restarts on
// exit (throttled to every 10 s) and appends output to the log files.
func RenderLaunchAgent(s Spec) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + escapeXML(s.Label) + `</string>

    <key>ProgramArguments</key>
    <array>
`)
	for _, a := range append([]string{s.Program}, s.Args...) {
		b.WriteString("        <string>" + escapeXML(a) + "</string>\n")
	}
	b.WriteString(`    </array>

    <key>WorkingDirectory</key>
    <string>` + escapeXML(s.WorkDir) + `</string>

    <key>EnvironmentVariables</key>
    <dict>
`)
	for _, e := range s.Env {
		b.WriteString("        <key>" + escapeXML(e.Key) + "</key>\n        <string>" + escapeXML(e.Value) + "</string>\n")
	}
	b.WriteString(`    </dict>

    <key>KeepAlive</key>
    <true/>

    <key>RunAtLoad</key>
    <true/>

    <key>ThrottleInterval</key>
    <integer>10</integer>

    <key>StandardOutPath</key>
    <string>` + escapeXML(filepath.Join(s.LogDir, s.LogName+".log")) + `</string>

    <key>StandardErrorPath</key>
    <string>` + escapeXML(filepath.Join(s.LogDir, s.LogName+".error.log")) + `</string>
</dict>
</plist>
`)
	return b.String()
}

func quoteSystemd(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// RenderSystemdUnit is the systemd user unit with the same restart policy.
func RenderSystemdUnit(s Spec) string {
	exec := quoteSystemd(s.Program)
	for _, a := range s.Args {
		exec += " " + quoteSystemd(a)
	}
	lines := []string{
		"[Unit]",
		"Description=" + s.Description,
		"After=network.target",
		"",
		"[Service]",
		"Type=simple",
		"WorkingDirectory=" + s.WorkDir,
		"ExecStart=" + exec,
		"Restart=always",
		"RestartSec=10",
	}
	if s.EnvFile != "" {
		lines = append(lines, "EnvironmentFile=-"+s.EnvFile)
	}
	for _, e := range s.Env {
		lines = append(lines, "Environment="+e.Key+"="+quoteSystemd(e.Value))
	}
	lines = append(lines,
		"StandardOutput=append:"+filepath.Join(s.LogDir, s.LogName+".log"),
		"StandardError=append:"+filepath.Join(s.LogDir, s.LogName+".error.log"),
		"",
		"[Install]",
		"WantedBy=default.target",
		"",
	)
	return strings.Join(lines, "\n")
}
