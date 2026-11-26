package update

import (
	"fmt"

	"github.com/NeoLabs-Systems/neolabs-go/cli"
	"github.com/NeoLabs-Systems/neolabs-go/cli/release"
)

// The closing lines of `update`, the same in every app.

// UpToDate says there is nothing to install.
func UpToDate(ui *cli.UI, version, channel string) {
	ui.Blank()
	ui.OK(fmt.Sprintf("You have the latest version (%s, %s)", version, release.NormalizeChannel(channel)))
	ui.Blank()
}

// Ready says the new version runs, where, and what was kept.
func Ready(ui *cli.UI, app, version, url, kept string) {
	ui.Blank()
	ui.Item("ok", ui.Bold(fmt.Sprintf("%s %s is ready", app, version)), url)
	ui.Blank()
	ui.Note(kept)
	ui.Blank()
}
