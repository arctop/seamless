package gardener

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"slices"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// Where a switch's value came from, as the console and doctor name it.
const (
	SourceDefault = "default" // nothing set it: agents may apply, nothing applies by itself
	SourceConfig  = "config"  // seamless.yaml or a SEAMLESS_GARDENER_* variable
	SourceConsole = "console" // the console's stored override (Settings, Gardener)
)

// AutoSafe reports whether automatic apply may take a proposal of kind: one of
// config.GardenerAutoApplyKinds, the one list (memory_wanted, tool_error,
// abandon_plan, ship_plan). Each is reversible from the console, needs no
// LLM, and only adds or retags. config validates gardener.auto_apply against
// the same list, which a test pins to this.
func AutoSafe(kind string) bool { return slices.Contains(config.GardenerAutoApplyKinds, kind) }

// AutoSafeKinds lists the kinds automatic apply may take, in
// store.ProposalKinds order.
func AutoSafeKinds() []string { return slices.Clone(config.GardenerAutoApplyKinds) }

// Switches are the gardener's two switches in force: the file/env config with
// the console's stored override layered on (Effective).
type Switches struct {
	// AgentsMayApply is whether agents may resolve proposals: gardener_apply's
	// apply, dismiss and hide, and the console's Gardener writes from an
	// agent's process. AgentsSource names who decided it, and AgentsLocked is
	// true when the console may not turn it on: the file or environment set
	// gardener.agents_may_apply: false.
	AgentsMayApply bool
	AgentsSource   string
	AgentsLocked   bool
	// AutoApply lists the kinds a pass applies by itself the moment it raises
	// one, in store.ProposalKinds order; empty applies nothing by itself.
	// AutoSource names who decided it.
	AutoApply  []string
	AutoSource string
	// Unreadable reports that a stored override exists but could not be read
	// (store.ErrGardenerOverrideInvalid): the file/env values decide, and the
	// next save from the console replaces the row.
	Unreadable bool
}

// Overridden reports whether a console choice is in force for either switch,
// which a reset clears.
func (sw Switches) Overridden() bool {
	return sw.AgentsSource == SourceConsole || sw.AutoSource == SourceConsole
}

// AutoApplies reports whether a new proposal of kind is applied by itself.
func (sw Switches) AutoApplies(kind string) bool {
	return AutoSafe(kind) && slices.Contains(sw.AutoApply, kind)
}

// Effective merges the file/env gardener config with the console's override.
// A field the override sets simply wins over the file until the owner resets
// it, as the briefing and features overrides do, with one exception, the one
// update.check has: an explicit file/env agents_may_apply: false is final.
// The console cannot turn it on, and shows the switch locked with where it was
// set. With the key unset, and no override, agents may apply: the behavior
// before the switch existed. AutoApply keeps only AutoSafe kinds, once each, in
// store.ProposalKinds order; with neither the file nor the console naming one,
// nothing applies by itself.
func Effective(base config.Gardener, o config.GardenerOverride) Switches {
	var sw Switches
	switch {
	case base.AgentsMayApply != nil && !*base.AgentsMayApply:
		sw.AgentsMayApply, sw.AgentsSource, sw.AgentsLocked = false, SourceConfig, true
	case o.AgentsMayApply != nil:
		sw.AgentsMayApply, sw.AgentsSource = *o.AgentsMayApply, SourceConsole
	case base.AgentsMayApply != nil:
		sw.AgentsMayApply, sw.AgentsSource = true, SourceConfig
	default:
		sw.AgentsMayApply, sw.AgentsSource = true, SourceDefault
	}
	kinds := base.AutoApply
	switch {
	case o.AutoApply != nil:
		kinds, sw.AutoSource = *o.AutoApply, SourceConsole
	case len(base.AutoApply) > 0:
		sw.AutoSource = SourceConfig
	default:
		sw.AutoSource = SourceDefault
	}
	sw.AutoApply = []string{}
	for _, k := range config.GardenerAutoApplyKinds {
		if slices.Contains(kinds, k) {
			sw.AutoApply = append(sw.AutoApply, k)
		}
	}
	return sw
}

// ReadSwitches reads the console's stored override from db and merges it
// with base (Effective). Every check calls it when it decides, so a change
// from the console takes effect without a restart. A stored row that cannot
// be read is no choice at all: it is logged, the file/env values decide, and
// Unreadable says so. Any other error is returned, and the caller decides
// nothing on it: an agent's apply is refused, and nothing is applied by
// itself.
func ReadSwitches(ctx context.Context, db *sql.DB, base config.Gardener, logger *slog.Logger) (Switches, error) {
	o, _, err := store.GardenerOverride(ctx, db)
	unreadable := false
	if err != nil {
		if !errors.Is(err, store.ErrGardenerOverrideInvalid) {
			return Switches{}, err
		}
		if logger != nil {
			logger.Warn("gardener: the stored switches override is unreadable; following the config file", "error", err)
		}
		o, unreadable = config.GardenerOverride{}, true
	}
	sw := Effective(base, o)
	sw.Unreadable = unreadable
	return sw, nil
}

// Switches is ReadSwitches over this service's store and the file/env values
// it was built with (FromConfig).
func (s *Service) Switches(ctx context.Context) (Switches, error) {
	return ReadSwitches(ctx, s.db, config.Gardener{AgentsMayApply: s.cfg.AgentsMayApply, AutoApply: s.cfg.AutoApply}, s.logger)
}
