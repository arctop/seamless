package console

// Settings > Backups' routes (console.go registers them, every one behind
// ownerHands): save and reset the schedule, Back up now and Try again, delete
// a backup, the folder field's live check, and a restore -- its passkey
// ceremony under the gate, staging it, cancelling a staged one, and
// dismissing the last one's result. The view is settings_backups.go.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/backup"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// backupsSave is what a save of the schedule form reads.
type backupsSave struct {
	Auto  bool
	Every time.Duration
	Keep  int
	// Dir is absolute and cleaned; LocalDir stands for Seamless's own folder.
	Dir string
}

// backupsFromForm reads the schedule form: auto (a checkbox, "on" when
// ticked), every (a duration within [1h, 720h]), keep (1 to 100) and dir (a
// full path, ~/ allowed; empty is Seamless's own folder). Each refusal names
// the field and what it accepts.
func backupsFromForm(form url.Values, local string) (backupsSave, string) {
	var out backupsSave
	if v, present := form["auto"]; present {
		if len(v) != 1 || (v[0] != "on" && v[0] != "1") {
			return out, "auto must be on, or left out for off"
		}
		out.Auto = true
	}
	one := func(name string) (string, string) {
		v, present := form[name]
		switch {
		case !present:
			return "", name + " is required"
		case len(v) != 1:
			return "", name + " must be given exactly once"
		}
		return strings.TrimSpace(v[0]), ""
	}
	raw, refusal := one("every")
	if refusal != "" {
		return out, refusal
	}
	d, err := config.ParseDuration(raw)
	if err != nil || d.Std() < config.MinBackupEvery || d.Std() > config.MaxBackupEvery {
		return out, fmt.Sprintf("every %q is not an interval between %s and %s", raw, config.Duration(config.MinBackupEvery), config.Duration(config.MaxBackupEvery))
	}
	out.Every = d.Std()
	raw, refusal = one("keep")
	if refusal != "" {
		return out, refusal
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < config.MinBackupKeep || n > config.MaxBackupKeep {
		return out, fmt.Sprintf("keep %q is not a number from %d to %d", raw, config.MinBackupKeep, config.MaxBackupKeep)
	}
	out.Keep = n
	raw, refusal = one("dir")
	if refusal != "" {
		return out, refusal
	}
	dir, refusal := backupDirField(raw, local)
	if refusal != "" {
		return out, refusal
	}
	out.Dir = dir
	return out, ""
}

// backupDirField reads a folder the owner typed: a full path, a leading ~/
// expanded to this daemon's home, cleaned; empty is Seamless's own folder.
func backupDirField(raw, local string) (string, string) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		return local, ""
	case len(raw) > maxBackupDirLen:
		return "", "the folder's path is too long"
	case raw == "~" || strings.HasPrefix(raw, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "~ cannot be used here: write the folder's full path"
		}
		raw = filepath.Join(home, strings.TrimPrefix(raw[1:], "/"))
	}
	if !filepath.IsAbs(raw) {
		return "", "the folder must be a full path, like /Volumes/Backup/Seamless Backups"
	}
	return filepath.Clean(raw), ""
}

// backupOverrideForSave reads the stored override a save is about to change.
// A row that does not decode, or holds a value out of range, cannot be kept:
// the service already ignores it, so the save replaces it (replaced).
func (s *Service) backupOverrideForSave(ctx context.Context) (config.BackupOverride, bool, error) {
	o, _, err := store.BackupOverride(ctx, s.writeDB())
	if err == nil {
		return o, false, nil
	}
	if _, _, gerr := store.GetSetting(ctx, s.writeDB(), store.SettingBackupConfig); gerr != nil {
		return config.BackupOverride{}, false, err
	}
	s.logger.Warn("console: replacing an unreadable backup override", "error", err)
	return config.BackupOverride{}, true, nil
}

// settingsBackupsSave stores the schedule form. Only what differs from the
// settings in force is stored, so a value the config file sets keeps following
// the file until the owner changes it here. A new folder is made and probed
// first (PrepareFolder); one that cannot be used is refused with nothing
// stored.
func (s *Service) settingsBackupsSave(w http.ResponseWriter, r *http.Request) {
	bv := s.cfg.Backups
	if bv == nil {
		backupsFlash(w, r, backupsNotRunning)
		return
	}
	local := filepath.Clean(backup.LocalDir(s.cfg.DataDir))
	f, refusal := backupsFromForm(r.PostForm, local)
	if refusal != "" {
		backupsFlash(w, r, capitalize(refusal)+": nothing was saved.")
		return
	}
	ctx := r.Context()
	set := bv.Status().Settings
	o, replaced, err := s.backupOverrideForSave(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	changed := false
	if f.Auto != set.Auto {
		on := f.Auto
		o.Auto, changed = &on, true
	}
	if f.Every != set.Every {
		d := config.Duration(f.Every)
		o.Every, changed = &d, true
	}
	if f.Keep != set.Keep {
		k := f.Keep
		o.Keep, changed = &k, true
	}
	moved := f.Dir != set.Dir
	if moved {
		if c := bv.PrepareFolder(f.Dir); !c.Usable() {
			backupsFlash(w, r, "The folder was not saved: "+c.Message()+".")
			return
		}
		dir := f.Dir
		if dir == local {
			dir = "" // Seamless's own folder, whatever the file names
		}
		o.Dir, changed = &dir, true
	}
	if !changed && !replaced {
		backupsNotice(w, r, "Nothing changed: these are the backup settings in force.")
		return
	}
	if err := store.SetBackupOverride(ctx, s.writeDB(), o); err != nil {
		backupsFlash(w, r, "The backup settings were not saved: "+err.Error())
		return
	}
	st, err := bv.Refresh(ctx)
	if err != nil {
		backupsRefreshFailed(w, r, "Saved.", err)
		return
	}
	msg := "Saved. " + backupsLead(st.Settings)
	if st.Settings.Auto && !set.Auto && !st.NextAt.IsZero() {
		msg += " The first one runs " + untilPhrase(st.NextAt, time.Now()) + "."
	}
	if moved {
		msg += " New backups go to " + st.Settings.Dir + ". The backups already in the old folder stay there."
	}
	if replaced {
		msg += " The stored backup setting was unreadable and has been replaced."
	}
	backupsNotice(w, r, msg)
}

// settingsBackupsReset clears the console's override, so the config file and
// environment (or, without either, the build) decide again.
func (s *Service) settingsBackupsReset(w http.ResponseWriter, r *http.Request) {
	bv := s.cfg.Backups
	if bv == nil {
		backupsFlash(w, r, backupsNotRunning)
		return
	}
	ctx := r.Context()
	if err := store.ClearBackupOverride(ctx, s.writeDB()); err != nil {
		s.serverError(w, r, err)
		return
	}
	st, err := bv.Refresh(ctx)
	if err != nil {
		backupsRefreshFailed(w, r, "The backup settings follow the config file again.", err)
		return
	}
	backupsNotice(w, r, "The backup settings follow the config file again. "+backupsLead(st.Settings)+
		" Backups go to "+st.Settings.Dir+".")
}

// backupsRefreshFailed answers a stored change the service could not pick up
// at once: a stopped service reads it when Seamless next starts.
func backupsRefreshFailed(w http.ResponseWriter, r *http.Request, saved string, err error) {
	if errors.Is(err, backup.ErrNotRunning) {
		backupsNotice(w, r, saved+" The backup service is not running, so the change applies when Seamless next starts.")
		return
	}
	backupsFlash(w, r, saved+" The backup service has not picked the change up yet: "+err.Error())
}

// settingsBackupsNow is Back up now (run "manual" or left out), or Try again
// (run "retry": the automatic backup, now, skipped when nothing changed).
func (s *Service) settingsBackupsNow(w http.ResponseWriter, r *http.Request) {
	bv := s.cfg.Backups
	if bv == nil {
		backupsFlash(w, r, backupsNotRunning)
		return
	}
	run := r.PostForm.Get("run")
	var err error
	switch run {
	case "", "manual":
		_, err = bv.BackUpNow(r.Context())
	case "retry":
		_, err = bv.RetryNow(r.Context())
	default:
		backupsFlash(w, r, fmt.Sprintf("run %q is not one of: manual, retry", run))
		return
	}
	switch {
	case errors.Is(err, backup.ErrBusy):
		msg := "A backup is already running. This section shows when it finishes."
		if st := bv.Status(); st.Running == nil && (st.Restore.Staging != nil || st.Restore.Archiving) {
			msg = "A restore is being prepared. Back up once it is done."
		}
		backupsFlash(w, r, msg)
	case errors.Is(err, backup.ErrNotRunning):
		backupsFlash(w, r, backupsStopped)
	case err != nil && r.Context().Err() != nil:
		backupsFlash(w, r, "Stopped waiting for the backup service. The backup carries on; this section shows how it went.")
	case err != nil:
		backupsFlash(w, r, "The backup did not start: "+err.Error())
	case run == "retry":
		backupsNotice(w, r, "Trying the automatic backup again now. This section shows how it goes.")
	default:
		backupsNotice(w, r, "Backing up now. This section shows when it is done.")
	}
}

// settingsBackupsDelete deletes one listed backup (field ref, as the list
// names it). The form asks for a confirm first.
func (s *Service) settingsBackupsDelete(w http.ResponseWriter, r *http.Request) {
	bv := s.cfg.Backups
	if bv == nil {
		backupsFlash(w, r, backupsNotRunning)
		return
	}
	ref := strings.TrimSpace(r.PostForm.Get("ref"))
	if ref == "" {
		backupsFlash(w, r, "ref is required: name the backup to delete.")
		return
	}
	name := ref
	if _, n, ok := strings.Cut(ref, ":"); ok {
		name = n
	}
	err := bv.Delete(r.Context(), ref, restoreProofOwner)
	switch {
	case errors.Is(err, backup.ErrNoSuchBackup):
		backupsFlash(w, r, "That backup is not in the list any more, so nothing was deleted.")
	case err != nil:
		backupsFlash(w, r, "The backup could not be deleted: "+err.Error())
	default:
		backupsNotice(w, r, "Deleted "+name+".")
	}
}

// settingsBackupsFolder answers the folder field's live check, as JSON: a look
// only (stats, never a write). Anything inside Seamless's data folder is
// answered as such before any other probe.
func (s *Service) settingsBackupsFolder(w http.ResponseWriter, r *http.Request) {
	bv := s.cfg.Backups
	if bv == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": backupsNotRunning})
		return
	}
	values, present := r.URL.Query()["dir"]
	if !present || len(values) != 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "dir is required, exactly once"})
		return
	}
	local := filepath.Clean(backup.LocalDir(s.cfg.DataDir))
	dir, refusal := backupDirField(values[0], local)
	if refusal != "" {
		writeJSON(w, http.StatusOK, backupsFolderLine{State: string(backup.FolderNotAbsolute), Text: capitalize(refusal), Dir: values[0]})
		return
	}
	writeJSON(w, http.StatusOK, folderLine(bv.CheckFolder(dir), local))
}

// restoreFields reads what every restore route shares: ref (as the list names
// it) and mode (replace or merge).
func restoreFields(form url.Values) (string, approval.RestoreMode, string) {
	ref := strings.TrimSpace(form.Get("ref"))
	if ref == "" {
		return "", "", "ref is required: name the backup to restore"
	}
	mode, err := approval.ParseRestoreMode(form.Get("mode"))
	if err != nil {
		return "", "", "mode " + strconv.Quote(form.Get("mode")) + " is not one of: replace, merge"
	}
	return ref, mode, ""
}

// settingsBackupsRestoreBegin starts the passkey ceremony that approves one
// restore, answering its id and the navigator.credentials.get options. Only a
// browser begins one, and only where the gate holds; the digest is the
// server's, read from the file.
func (s *Service) settingsBackupsRestoreBegin(w http.ResponseWriter, r *http.Request) {
	bv := s.cfg.Backups
	if bv == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": backupsNotRunning})
		return
	}
	ctx := r.Context()
	if !s.restoreGated(ctx) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this install needs no passkey to restore: use Restore"})
		return
	}
	if s.classify(r) != credCookie {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "a restore here needs a passkey: open this page in the browser you sign in to the console with"})
		return
	}
	ref, mode, refusal := restoreFields(r.PostForm)
	if refusal != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": refusal})
		return
	}
	e, digest, err := bv.Digest(ctx, ref)
	if err != nil {
		s.restoreStageError(w, r, err)
		return
	}
	if f := backup.Restorable(e, string(mode)); f != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": f.Message})
		return
	}
	begun, err := s.cfg.Approvals.BeginRestore(ctx, digest, mode)
	if err != nil {
		s.restoreApprovalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, begun)
}

// settingsBackupsRestore stages a restore. Under the gate it needs the
// ceremony's id and the browser's response: the digest is read from the file
// again and must be the one the ceremony began over, and the stage expects
// it, so a file that changed is refused at every step.
func (s *Service) settingsBackupsRestore(w http.ResponseWriter, r *http.Request) {
	bv := s.cfg.Backups
	if bv == nil {
		restoreRefused(w, r, http.StatusNotFound, backupsNotRunning)
		return
	}
	ref, mode, refusal := restoreFields(r.PostForm)
	if refusal != "" {
		restoreRefused(w, r, http.StatusBadRequest, capitalize(refusal)+".")
		return
	}
	ctx := r.Context()
	req := backup.RestoreRequest{Ref: ref, Mode: string(mode), By: restoreBy, Proof: restoreProofOwner}
	if s.restoreGated(ctx) {
		if s.classify(r) != credCookie {
			restoreRefused(w, r, http.StatusForbidden, "A restore here needs a passkey: open this page in the browser you sign in to the console with, or restore from a terminal.")
			return
		}
		id, response := strings.TrimSpace(r.PostForm.Get("id")), r.PostForm.Get("response")
		if id == "" || response == "" {
			restoreRefused(w, r, http.StatusForbidden, "A restore here needs your passkey: use Restore with passkey, or the terminal command this section shows.")
			return
		}
		_, digest, err := bv.Digest(ctx, ref)
		if err != nil {
			s.restoreStageError(w, r, err)
			return
		}
		row, err := s.cfg.Approvals.FinishRestore(ctx, id, digest, mode, []byte(response))
		if err != nil {
			s.restoreApprovalError(w, r, err)
			return
		}
		req.ExpectSHA256, req.Proof, req.Credential = digest, approval.ProofPasskey, row.ID
	}
	if _, err := bv.StageRestore(ctx, req); err != nil {
		s.restoreStageError(w, r, err)
		return
	}
	msg := "Restoring the backup. This section shows each step; Seamless restarts to finish."
	if !restartsInPlace() {
		msg = "The restore is being prepared. Once it is ready, restart Seamless to finish: seamlessd restart."
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"staging": true, "mode": string(mode), "notice": msg})
		return
	}
	backupsNotice(w, r, msg)
}

// restoreStageError answers a restore the backup service refused.
func (s *Service) restoreStageError(w http.ResponseWriter, r *http.Request, err error) {
	var f *backup.Failure
	switch {
	case errors.Is(err, backup.ErrNoSuchBackup):
		restoreRefused(w, r, http.StatusNotFound, "That backup is not in the list any more.")
	case errors.Is(err, backup.ErrBusy):
		restoreRefused(w, r, http.StatusConflict, "A backup or a restore is under way. Restore once it finishes.")
	case errors.Is(err, backup.ErrNotRunning):
		restoreRefused(w, r, http.StatusConflict, backupsStopped)
	case errors.As(err, &f):
		restoreRefused(w, r, http.StatusConflict, nothingChanged(f.Message))
	case r.Context().Err() != nil:
		restoreRefused(w, r, http.StatusServiceUnavailable, "Stopped waiting for the backup service. Nothing was started; try again.")
	default:
		s.logger.Error("console: backups: restore", "error", err)
		restoreRefused(w, r, http.StatusInternalServerError, "The restore did not start: "+err.Error())
	}
}

// restoreApprovalError answers a refused ceremony in the owner's words.
func (s *Service) restoreApprovalError(w http.ResponseWriter, r *http.Request, err error) {
	msg, status := "", http.StatusConflict
	switch {
	case errors.Is(err, approval.ErrNoRestore):
		msg = "this approval expired or was already used, or the backup changed since it began; start again"
	case errors.Is(err, approval.ErrNoPasskey):
		msg = "no passkey is enrolled: restore from a terminal with the command this section shows, or add a passkey on the Approvers page"
	case errors.Is(err, approval.ErrPasskeysOff):
		msg = "passkeys are not configured on this daemon: restore from a terminal with the command this section shows"
	case errors.Is(err, approval.ErrInvalidRestoreMode):
		msg, status = err.Error(), http.StatusBadRequest
	case errors.Is(err, approval.ErrTooManyRestores):
		msg, status = "too many restore approvals are waiting; try again in a few minutes", http.StatusTooManyRequests
	case errors.Is(err, approval.ErrRestricted):
		msg, status = "forbidden: a restricted local account restores nothing", http.StatusForbidden
	}
	if msg != "" {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	s.approverError(w, r, err)
}

// settingsBackupsRestoreCancel drops a staged restore that has not begun to
// apply.
func (s *Service) settingsBackupsRestoreCancel(w http.ResponseWriter, r *http.Request) {
	bv := s.cfg.Backups
	if bv == nil {
		backupsFlash(w, r, backupsNotRunning)
		return
	}
	waiting := bv.Status().Restore.Pending != nil
	_, err := bv.CancelRestore(r.Context())
	var f *backup.Failure
	switch {
	case errors.As(err, &f):
		backupsFlash(w, r, sentenceStart(f.Message)+".")
	case errors.Is(err, backup.ErrNotRunning):
		backupsFlash(w, r, backupsStopped)
	case err != nil:
		backupsFlash(w, r, "The restore could not be cancelled: "+err.Error())
	case !waiting:
		backupsNotice(w, r, "No restore was waiting to finish.")
	default:
		backupsNotice(w, r, "The restore was cancelled. Nothing changed.")
	}
}

// settingsBackupsRestoreDismiss hides the last restore's banner (and a failed
// preparation's). It waits while the replaced data is still being saved.
func (s *Service) settingsBackupsRestoreDismiss(w http.ResponseWriter, r *http.Request) {
	bv := s.cfg.Backups
	if bv == nil {
		backupsFlash(w, r, backupsNotRunning)
		return
	}
	_, err := bv.DismissResult(r.Context())
	var f *backup.Failure
	switch {
	case errors.As(err, &f):
		backupsFlash(w, r, sentenceStart(f.Message)+": dismiss it once that is done.")
	case errors.Is(err, backup.ErrNotRunning):
		backupsFlash(w, r, backupsStopped)
	case err != nil:
		backupsFlash(w, r, "The result could not be dismissed: "+err.Error())
	default:
		http.Redirect(w, r, backupsSectionHref+"#backups", http.StatusSeeOther)
	}
}

// restoreRefused answers a refused restore: JSON for the section's script
// (and a script with the key), else the section with an error flash.
func restoreRefused(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if wantsJSON(r) {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	backupsFlash(w, r, msg)
}

func backupsFlash(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "backups", "error", msg, "backups")
}

func backupsNotice(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "backups", "notice", msg, "backups")
}
