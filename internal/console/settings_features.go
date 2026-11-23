package console

// The features form's routes (Settings > Features); the cards are
// features.go's.

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/store"
)

// settingsFeaturesSave persists the optional-features form as the stored
// override row. Like the briefing form it rebuilds the WHOLE struct from
// checkbox presence -- an unchecked box submits nothing, so reading only the
// fields that arrived would make "turn it off" indistinguishable from "leave it
// alone". It never rewrites the config file, and it applies immediately in the
// console (agents pick the change up when they next list tools).
func (s *Service) settingsFeaturesSave(w http.ResponseWriter, r *http.Request) {
	// Boundary check before anything is stored: a field that is present but not
	// the checkbox's own value, or a feature_* field naming nothing in the
	// registry, is a caller mistake and must not quietly become "off".
	known := make(map[string]bool, len(features.Registry()))
	for _, f := range features.Registry() {
		known[featureField(f.Key)] = true
	}
	for name, values := range r.PostForm {
		if !strings.HasPrefix(name, featureFieldPrefix) {
			continue
		}
		if !known[name] {
			settingsFeaturesFlash(w, r, fmt.Sprintf("unknown feature field %q", name))
			return
		}
		if len(values) != 1 || values[0] != "1" {
			settingsFeaturesFlash(w, r, fmt.Sprintf("%s must be omitted (off) or set exactly once to 1 (on)", name))
			return
		}
	}

	var cfg config.Features
	changed := make(map[string]any, len(features.Registry()))
	var on []string
	for _, f := range features.Registry() {
		enabled := r.PostFormValue(featureField(f.Key)) != ""
		f.Set(&cfg, enabled)
		changed[string(f.Key)] = enabled
		if enabled {
			on = append(on, f.Label)
		}
	}
	if err := store.SetFeaturesConfig(r.Context(), s.writeDB(), cfg); err != nil {
		s.serverError(w, r, err)
		return
	}
	if s.cfg.Events != nil {
		if _, err := s.eventLog(r.Context()).Record(r.Context(), core.Event{
			Kind:    eventFeaturesChanged,
			Payload: map[string]any{"features": changed, "by": "console"},
		}); err != nil {
			s.logger.Warn("console: record features event", "error", err)
		}
	}
	msg := "Optional features saved: all of them are off."
	if len(on) > 0 {
		msg = "Optional features saved: " + strings.Join(on, ", ") +
			" on. Agents pick the change up on their next session."
	}
	settingsFeaturesNotice(w, r, msg)
}

// settingsFeaturesReset clears the stored override row, so the effective state
// falls back to the file/env configuration -- which, unless the owner set the
// keys there, means every optional feature is off again. Nothing is deleted:
// the feature's data is untouched either way.
func (s *Service) settingsFeaturesReset(w http.ResponseWriter, r *http.Request) {
	if err := store.ClearFeaturesConfig(r.Context(), s.writeDB()); err != nil {
		s.serverError(w, r, err)
		return
	}
	if s.cfg.Events != nil {
		if _, err := s.eventLog(r.Context()).Record(r.Context(), core.Event{
			Kind:    eventFeaturesChanged,
			Payload: map[string]any{"reset": true, "by": "console"},
		}); err != nil {
			s.logger.Warn("console: record features event", "error", err)
		}
	}
	settingsFeaturesNotice(w, r, "Feature override cleared: back to the file/env configuration.")
}

func settingsFeaturesFlash(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "features", "error", msg, "features")
}

func settingsFeaturesNotice(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "features", "notice", msg, "features")
}
