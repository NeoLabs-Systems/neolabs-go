package update

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/NeoLabs-Systems/neolabs-go/cli"
)

func TestApplyRollsBackOnlyAfterTheSwitch(t *testing.T) {
	boom := errors.New("boom")
	for _, c := range []struct {
		failAt string
		want   []string
		code   string
	}{
		{"", []string{"backup", "download", "install", "restart", "verify"}, ""},
		{"download", []string{"backup", "download"}, ""},
		{"install", []string{"backup", "download", "install"}, ""},
		{"restart", []string{"backup", "download", "install", "restart", "rollback"}, CodeRolledBack},
		{"verify", []string{"backup", "download", "install", "restart", "verify", "rollback"}, CodeRolledBack},
	} {
		var ran []string
		step := func(name string) func(context.Context) error {
			return func(context.Context) error {
				ran = append(ran, name)
				if name == c.failAt {
					return boom
				}
				return nil
			}
		}
		err := Apply(context.Background(), cli.NewUI(true), Plan{App: "App", From: "1", To: "2",
			Backup: step("backup"), Download: step("download"), Install: step("install"),
			Restart: step("restart"), Verify: step("verify"), Rollback: step("rollback")})
		if !slices.Equal(ran, c.want) || (c.failAt != "") != errors.Is(err, boom) || cli.CodeOf(err, "") != c.code {
			t.Errorf("fail at %q: ran %v, code %q, err %v", c.failAt, ran, cli.CodeOf(err, ""), err)
		}
	}
}
