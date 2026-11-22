package config

import (
	"fmt"
	"slices"
	"strings"
)

// GardenerAutoApplyKinds are the proposal kinds automatic apply may take
// (gardener.auto_apply), in store.ProposalKinds order. This is the one list:
// gardener.AutoSafe reads it, and a gardener test pins it to the store's kind
// names and to the owner's rule. Each is reversible from the console, needs
// no LLM, and only adds or retags: memory_wanted and tool_error open a task,
// abandon_plan and ship_plan retag a captured plan's note. Every other kind
// (archive, merge, consolidate, digest, reproject, relocate, rekind, split,
// merge_plans) is a judgement call, and consolidate and split cannot be
// undone, so the owner reviews each of those.
var GardenerAutoApplyKinds = []string{"abandon_plan", "memory_wanted", "tool_error", "ship_plan"}

// ValidateGardenerAutoApply refuses a kind automatic apply may not take,
// naming where it was set (key: gardener.auto_apply, the console's form or its
// stored row), the kind, why, and the valid values. A kind named twice is
// accepted; the effective list holds it once.
func ValidateGardenerAutoApply(key string, kinds []string) error {
	for _, k := range kinds {
		if !slices.Contains(GardenerAutoApplyKinds, k) {
			return fmt.Errorf("config: %s: %q cannot be applied automatically: only kinds that are reversible, "+
				"need no LLM and only add or retag may be, and every other kind waits for the owner's review: valid values are %s",
				key, k, strings.Join(GardenerAutoApplyKinds, ", "))
		}
	}
	return nil
}

// GardenerOverride is the console's runtime override of the gardener's two
// switches, stored as JSON in the settings table (store.GardenerOverride). A
// nil field is no override: that switch follows the file/env value. The merge
// is gardener.Effective: a set field wins over the file until the owner
// resets it, except that agents_may_apply: false in the file or environment
// is final. AutoApply is a pointer so that "this console chose no kinds"
// (an empty list) differs from "no choice made here" (nil).
type GardenerOverride struct {
	AgentsMayApply *bool     `json:"agents_may_apply,omitempty"`
	AutoApply      *[]string `json:"auto_apply,omitempty"`
}

// Validate refuses an override automatic apply could not honor: a kind
// outside GardenerAutoApplyKinds.
func (o GardenerOverride) Validate() error {
	if o.AutoApply == nil {
		return nil
	}
	return ValidateGardenerAutoApply("the console's gardener override", *o.AutoApply)
}
