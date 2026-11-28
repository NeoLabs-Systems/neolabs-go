package account

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"

	"github.com/NeoLabs-Systems/neolabs-go/userid"
)

// AdminRule decides when an admin account may be deleted.
type AdminRule int

const (
	// KeepLastAdmin lets an admin be deleted while another active admin
	// remains.
	KeepLastAdmin AdminRule = iota
	// KeepAdmins never deletes an admin account; the role is removed first.
	KeepAdmins
)

// Step is one part of erasing an account, run in order inside the deletion's
// transaction. Steps erase rows and plan what happens after the commit. They
// must use tx only: the writer pool has one connection, which the
// transaction holds.
type Step func(ctx context.Context, tx *sql.Tx, d *Deletion) error

// Deletion is one account being deleted.
type Deletion struct {
	Account Account
	Actor   userid.ID // the deleting admin; empty when the account deletes itself
	// Result carries values the app's response reports, such as counts.
	Result map[string]any

	files []removal
	after []func(context.Context)
}

type removal struct{ root, name string }

// Self reports whether the account deletes itself.
func (d *Deletion) Self() bool { return d.Actor == "" }

// RemoveAfterCommit removes a file or directory tree once the rows are gone.
// name is relative to root, or an absolute path inside it; anything that
// resolves outside root is left alone.
func (d *Deletion) RemoveAfterCommit(root, name string) {
	d.files = append(d.files, removal{root, name})
}

// AfterCommit runs fn once the deletion committed, after files are removed:
// notify share partners, stop the account's runtime, clear caches.
func (d *Deletion) AfterCommit(fn func(ctx context.Context)) { d.after = append(d.after, fn) }

// Deleter erases accounts. The rows go in one transaction; sessions, files
// and the app's follow-up work come after it committed, so a failed erase
// leaves the account whole.
type Deleter struct {
	DB   *sql.DB // writer
	Rule AdminRule
	// ProtectAdminTargets makes DeleteByAdmin refuse admin accounts even
	// under KeepLastAdmin; the role is removed first.
	ProtectAdminTargets bool
	// OtherAdmins counts the active admins besides the account, inside the
	// transaction. Required with KeepLastAdmin.
	OtherAdmins func(ctx context.Context, tx *sql.Tx, id userid.ID) (int, error)
	Steps       []Step
	// Sessions are ended after the commit (sessions kept in another database
	// or not cascaded by the rows).
	Sessions Revoker
	Log      *slog.Logger
}

func (x *Deleter) log() *slog.Logger {
	if x.Log != nil {
		return x.Log
	}
	return slog.Default()
}

// Delete erases the account at its own request. Re-authenticate first.
func (x *Deleter) Delete(ctx context.Context, a Account) (*Deletion, error) {
	if a.Admin && x.Rule == KeepAdmins {
		return nil, ErrAdminSelfDelete
	}
	return x.run(ctx, &Deletion{Account: a})
}

// DeleteByAdmin erases another account for an admin.
func (x *Deleter) DeleteByAdmin(ctx context.Context, actor userid.ID, a Account) (*Deletion, error) {
	if actor == a.ID {
		return nil, ErrDeleteSelf
	}
	if a.Admin && (x.Rule == KeepAdmins || x.ProtectAdminTargets) {
		return nil, ErrAdminAccount
	}
	return x.run(ctx, &Deletion{Account: a, Actor: actor})
}

func (x *Deleter) run(ctx context.Context, d *Deletion) (*Deletion, error) {
	if len(x.Steps) == 0 {
		return nil, errors.New("account: no erase steps configured")
	}
	d.Result = map[string]any{}
	tx, err := x.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if d.Account.Admin && x.Rule == KeepLastAdmin {
		if x.OtherAdmins == nil {
			return nil, errors.New("account: OtherAdmins is required to delete an admin")
		}
		n, err := x.OtherAdmins(ctx, tx, d.Account.ID)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, ErrLastAdmin
		}
	}
	for _, step := range x.Steps {
		if err := step(ctx, tx, d); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	x.finish(context.WithoutCancel(ctx), d)
	return d, nil
}

func (x *Deleter) finish(ctx context.Context, d *Deletion) {
	if x.Sessions != nil {
		if err := x.Sessions.DestroyForUser(ctx, string(d.Account.ID)); err != nil {
			x.log().Error("account: sessions not revoked after deletion", "err", err)
		}
	}
	for _, f := range d.files {
		if err := removeInside(f.root, f.name); err != nil {
			x.log().Warn("account: file not removed after deletion", "err", err)
		}
	}
	for _, fn := range d.after {
		fn(ctx)
	}
}

// removeInside removes name below root without following it out of root.
func removeInside(root, name string) error {
	if root == "" || name == "" {
		return nil
	}
	if filepath.IsAbs(name) {
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		name = rel
	}
	if !filepath.IsLocal(name) {
		return fmt.Errorf("%q is outside %s", name, root)
	}
	r, err := os.OpenRoot(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer r.Close()
	return r.RemoveAll(name)
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// DeleteWhere erases the rows of each table whose column holds the account's
// id, e.g. DeleteWhere("user_id", "notes", "settings"), then
// DeleteWhere("id", "users") as the last step. The rows removed are added to
// Result["rowsDeleted"].
func DeleteWhere(column string, tables ...string) Step {
	return func(ctx context.Context, tx *sql.Tx, d *Deletion) error {
		for _, name := range append([]string{column}, tables...) {
			if !identifier.MatchString(name) {
				return fmt.Errorf("account: %q is not a plain identifier", name)
			}
		}
		for _, table := range tables {
			res, err := tx.ExecContext(ctx, `DELETE FROM "`+table+`" WHERE "`+column+`" = ?`, idArg(d.Account.ID))
			if err != nil {
				return fmt.Errorf("account: erase %s: %w", table, err)
			}
			n, _ := res.RowsAffected()
			total, _ := d.Result["rowsDeleted"].(int64)
			d.Result["rowsDeleted"] = total + n
		}
		return nil
	}
}
