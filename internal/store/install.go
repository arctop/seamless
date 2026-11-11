package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/arctop/seamless/internal/core"
)

// SettingInstallID is the settings key holding this install's id: a ULID minted
// the first time anything asks (InstallID), kept as it is across a restore
// (RestoreKeptSettings), and recorded in every backup the install writes. It is
// how a backup folder that several installs share -- even two machines with the
// same hostname -- tells their backups apart, so each prunes only its own.
const SettingInstallID = "install_id"

// InstallID returns this install's id, minting and storing one the first time.
// Two first calls racing agree: the insert keeps an id already there, and the
// id is read back after it.
func InstallID(ctx context.Context, db *sql.DB) (string, error) {
	const op = "store.InstallID"
	if id, found, err := GetSetting(ctx, db, SettingInstallID); err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	} else if found && strings.TrimSpace(id) != "" {
		return id, nil
	}
	id, err := core.NewID()
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	// A blank row (only a hand edit makes one) is replaced; an id already there
	// is kept, which is what makes a race between two first calls agree.
	if _, err := db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value WHERE trim(settings.value) = ''`, SettingInstallID, id); err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	got, found, err := GetSetting(ctx, db, SettingInstallID)
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	if !found || strings.TrimSpace(got) == "" {
		return "", fmt.Errorf("%s: the id did not stick", op)
	}
	return got, nil
}
