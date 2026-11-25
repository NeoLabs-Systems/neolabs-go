package cli

import "fmt"

// HomeActions describes the operations available from an application's home
// screen. A nil operation is omitted, allowing each app to supply its own setup.
type HomeActions struct {
	AppName                                                      string
	Running                                                      bool
	UpdateVersion                                                string
	ShowUpdate                                                   bool
	SettingsHint                                                 string
	Open, Start, Status, Restart, Update, Settings, Doctor, Help func() error
}

type homeAction struct {
	choice Choice
	run    func() error
}

func (h HomeActions) actions() []homeAction {
	var actions []homeAction
	add := func(label, hint string, run func() error) {
		if run != nil {
			actions = append(actions, homeAction{Choice{Label: label, Hint: hint}, run})
		}
	}
	if h.Running {
		add("Open "+h.AppName+" in my browser", "", h.Open)
	} else {
		add("Start "+h.AppName, "", h.Start)
	}
	if h.UpdateVersion != "" || h.ShowUpdate {
		hint := ""
		if h.UpdateVersion != "" {
			hint = h.UpdateVersion + " is ready"
		}
		add("Get the latest version", hint, h.Update)
	}
	add("Check that everything works", "", h.Status)
	if h.Running {
		add("Restart "+h.AppName, "", h.Restart)
	}
	add("Change settings", h.SettingsHint, h.Settings)
	add("Something's wrong — help me fix it", "", h.Doctor)
	add("More…", "every command", h.Help)
	return actions
}

// HomeMenu displays the same interactive menu and noninteractive help hint for
// every NeoLabs application, and invokes the selected app-provided operation.
func (u *UI) HomeMenu(command string, h HomeActions) error {
	if !u.Interactive() {
		u.Print("  " + u.Faint("Every command:") + " " + u.Command(command+" help"))
		u.Blank()
		return nil
	}
	actions := h.actions()
	if len(actions) == 0 {
		return nil
	}
	choices := make([]Choice, len(actions))
	for i := range actions {
		choices[i] = actions[i].choice
		choices[i].Value = fmt.Sprint(i)
	}
	selected, ok, err := u.SelectOrQuit("What would you like to do?", choices)
	if err != nil || !ok {
		return err
	}
	var index int
	if _, err := fmt.Sscan(selected, &index); err != nil || index < 0 || index >= len(actions) {
		return fmt.Errorf("invalid menu selection %q", selected)
	}
	return actions[index].run()
}
