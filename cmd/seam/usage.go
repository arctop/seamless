package main

// seam usage -- roll-up from the usage_summary MCP tool.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

var usageCmd = spec("usage", groupObservability, "activity roll-up",
	noArgs(), bindUsage, runUsage)

type usageOpts struct {
	asJSON *bool
}

func bindUsage(fs *flag.FlagSet) *usageOpts {
	return &usageOpts{asJSON: jsonFlag(fs, jsonUsage)}
}

func runUsage(ctx context.Context, e *env, o *usageOpts, _ []string) error {
	cli, _, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()

	if *o.asJSON {
		return printToolJSON(ctx, e, cli, "usage_summary", nil)
	}
	out, err := callTool(ctx, cli, "usage_summary", nil)
	if err != nil {
		return err
	}
	mem, _ := out["memories"].(map[string]any)
	ret, _ := out["retrieval"].(map[string]any)
	fmt.Fprintf(e.stdout, "memories: %d active\n", int(num(mem["active"])))
	printCounts(e.stdout, "  by kind", mem["byKind"])
	fmt.Fprintf(e.stdout, "notes:    %d\n", int(num(out["notes"])))
	printCounts(e.stdout, "sessions", out["sessions"])
	printCounts(e.stdout, "tasks", out["tasks"])
	fmt.Fprintf(e.stdout, "retrieval: %d injections, %d reads (%d%% read-after-inject)\n",
		int(num(ret["injections"])), int(num(ret["reads"])), pctOf(num(ret["reads"]), num(ret["injections"])))
	if top, ok := ret["topInjected"].([]any); ok && len(top) > 0 {
		fmt.Fprintln(e.stdout, "  most injected:")
		for _, t := range top {
			m, _ := t.(map[string]any)
			fmt.Fprintf(e.stdout, "    %-32s %dx\n", str(m["name"]), int(num(m["count"])))
		}
	}
	if top, ok := ret["topUtility"].([]any); ok && len(top) > 0 {
		fmt.Fprintln(e.stdout, "  highest utility (decayed demand):")
		for _, t := range top {
			m, _ := t.(map[string]any)
			fmt.Fprintf(e.stdout, "    %-32s %.2f\n", str(m["name"]), num(m["score"]))
		}
	}
	printCounts(e.stdout, "proposals", out["gardenerPending"])
	return nil
}

// printCounts renders a map[string]number as "label: k=v k=v".
func printCounts(w io.Writer, label string, v any) {
	m, _ := v.(map[string]any)
	if len(m) == 0 {
		fmt.Fprintf(w, "%s: (none)\n", label)
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, int(num(m[k]))))
	}
	fmt.Fprintf(w, "%s: %s\n", label, strings.Join(parts, " "))
}
