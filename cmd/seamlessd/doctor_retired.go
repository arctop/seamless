package main

import (
	"fmt"
	"strings"

	"github.com/arctop/seamless/internal/config"
)

// retiredKeysCheck is doctor's row for configuration that still names a
// feature switch Seamless no longer has: features.momentum and
// features.gamification in the file, or their SEAMLESS_FEATURES_* overrides
// in the environment. Momentum and gamification are always on now, so the
// keys parse (the file must keep loading) and do nothing; the row is info,
// naming each one so the owner can remove it. ok is false when nothing names
// one: no row.
func retiredKeysCheck(cfg config.Config, lookupEnv func(string) (string, bool)) (check, bool) {
	var named []string
	if keys := cfg.Features.RetiredKeys(); len(keys) > 0 {
		where := "the config file"
		if path := cfg.SourcePath(); path != "" {
			where = path
		}
		named = append(named, fmt.Sprintf("%s in %s", strings.Join(keys, " and "), where))
	}
	var env []string
	for _, name := range config.RetiredFeatureEnv {
		if _, set := lookupEnv(name); set {
			env = append(env, name)
		}
	}
	if len(env) > 0 {
		named = append(named, strings.Join(env, " and ")+" in the environment")
	}
	if len(named) == 0 {
		return check{}, false
	}
	return check{statusInfo, "retired keys", strings.Join(named, "; ") +
		": momentum and gamification are always on, so these switch nothing -- remove them"}, true
}
