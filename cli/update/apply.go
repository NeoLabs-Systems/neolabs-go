package update

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/NeoLabs-Systems/neolabs-go/cli"
)

// Plan is one update. Everything before Restart leaves the running app alone;
// a failure from Restart on is rolled back.
type Plan struct {
	App, From, To, Channel string
	Backup                 func(context.Context) error // nil when there is no data yet
	Download               func(context.Context) error
	Install                func(context.Context) error
	Restart                func(context.Context) error
	Verify                 func(context.Context) error
	// Rollback puts the previous version back and waits for it to answer.
	Rollback        func(context.Context) error
	RollbackTimeout time.Duration // default 90s
	// Progress is told each phase, for apps that report updates to a UI.
	Progress func(phase string, percent int, message string)
}

// Error codes of a failed update.
const (
	CodeRolledBack     = "UPDATE_ROLLED_BACK"
	CodeRollbackFailed = "UPDATE_ROLLBACK_FAILED"
)

// RolledBack reports whether err left the previous version running.
func RolledBack(err error) bool { return cli.CodeOf(err, "") == CodeRolledBack }

// Apply runs the plan with the NeoLabs terminal step list.
func Apply(ctx context.Context, ui *cli.UI, p Plan) error {
	title := fmt.Sprintf("%s → %s", p.From, p.To)
	if p.Channel != "" {
		title += " · " + p.Channel
	}
	ui.Title("Updating "+p.App, title)
	ui.Blank()

	type phase struct {
		name  string
		label cli.Label
		run   func(context.Context) error
	}
	var phases []phase
	if p.Backup != nil {
		phases = append(phases, phase{"backup", cli.Label{Doing: "Saving a backup of your data", Done: "Saved a backup of your data"}, p.Backup})
	}
	switching := len(phases) + 2
	phases = append(phases,
		phase{"downloading", cli.Label{Doing: "Downloading the new version", Done: "Downloaded the new version"}, p.Download},
		phase{"installing", cli.Label{Doing: "Installing it", Done: "Installed it"}, p.Install},
		phase{"restarting", cli.Label{Doing: "Restarting " + p.App, Done: "Restarted " + p.App}, p.Restart},
		phase{"verifying", cli.Label{Doing: "Making sure it all works", Done: "Made sure it all works"}, p.Verify},
	)
	labels := make([]cli.Label, len(phases))
	for i, ph := range phases {
		labels[i] = ph.label
	}
	list := ui.Steps(labels, true)
	defer list.Close()
	report := p.Progress
	if report == nil {
		report = func(string, int, string) {}
	}

	for i, ph := range phases {
		report(ph.name, 15+i*80/len(phases), ph.label.Doing)
		err := list.Run(i, func() error { return ph.run(ctx) })
		if err == nil {
			continue
		}
		if i < switching {
			return err
		}
		report("rollback", 85, fmt.Sprintf("Restoring %s after the update failed", p.From))
		back := list.Add(cli.Label{Doing: "Going back to " + p.From, Done: "Went back to " + p.From})
		rollbackErr := list.Run(back, func() error {
			timeout := p.RollbackTimeout
			if timeout <= 0 {
				timeout = 90 * time.Second
			}
			// The update's own context may be what failed it.
			rollbackCtx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			return p.Rollback(rollbackCtx)
		})
		if rollbackErr != nil {
			return &failure{code: CodeRollbackFailed, cause: errors.Join(err, rollbackErr),
				msg: fmt.Sprintf("%s\nGoing back to %s also failed: %s", err, p.From, rollbackErr)}
		}
		return &failure{code: CodeRolledBack, cause: err,
			msg: fmt.Sprintf("%s\n%s is running %s again; nothing was lost.", err, p.App, p.From)}
	}
	return nil
}

type failure struct {
	code, msg string
	cause     error
}

func (f *failure) Error() string { return f.msg }
func (f *failure) Code() string  { return f.code }
func (f *failure) Unwrap() error { return f.cause }
