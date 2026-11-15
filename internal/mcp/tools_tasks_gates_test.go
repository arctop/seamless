package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Task gates through the MCP tools: a step whose gate is open never appears
// in tasks_ready's ready list, clearing it makes the step ready without
// touching its status, a gate task appears only in the gates group, the
// filters answer the agent's and the owner's queues, and a task without a
// gate reads exactly as before.

// groupIDs returns the ids tasks_ready lists under key, in order.
func groupIDs(t *testing.T, out map[string]any, key string) []string {
	t.Helper()
	list, _ := out[key].([]any)
	ids := make([]string, 0, len(list))
	for _, r := range list {
		ids = append(ids, r.(map[string]any)["id"].(string))
	}
	return ids
}

// groupEntry returns the entry for id that tasks_ready lists under key.
func groupEntry(t *testing.T, out map[string]any, key, id string) map[string]any {
	t.Helper()
	for _, r := range out[key].([]any) {
		if m := r.(map[string]any); m["id"] == id {
			return m
		}
	}
	t.Fatalf("%s holds no %s", key, id)
	return nil
}

func TestTasksGates_ReadyGroupsAndClearing(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})

	free := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "free work", "body": "a long body"})["id"].(string)
	before := callJSON(t, ctx, cli, "tasks_ready", nil)
	require.Equal(t, []string{"project", "ready", "blocked"}, keysOf(before), "a queue without gates reads as it always has")
	require.NotContains(t, groupEntry(t, before, "ready", free), "gate")
	require.NotContains(t, groupEntry(t, before, "ready", free), "kind")

	gate := callJSON(t, ctx, cli, "tasks_add", map[string]any{
		"title": "GATE (owner): sign the contract", "kind": "gate",
		"gate_who": "owner", "gate_what": "sign the contract", "gate_decision": "which vendor?",
	})
	require.Equal(t, "gate", gate["kind"])
	require.Equal(t, map[string]any{"who": "owner", "what": "sign the contract", "decision": "which vendor?", "open": true}, gate["gate"])
	gateID := gate["id"].(string)
	behind := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "ship it", "depends_on": []string{gateID}})["id"].(string)
	own := callJSON(t, ctx, cli, "tasks_add", map[string]any{
		"title": "run the drill", "body": "drill body", "gate_who": "sitting:security", "gate_what": "the sitting runs the drill",
	})["id"].(string)

	out := callJSON(t, ctx, cli, "tasks_ready", nil)
	require.Equal(t, []string{free}, readyIDs(t, out), "a gate and every step it holds stay off ready")
	require.Empty(t, out["blocked"])
	require.Equal(t, []string{behind, own}, groupIDs(t, out, "gated"))
	require.Equal(t, []string{gateID}, groupIDs(t, out, "gates"))
	ownEntry := groupEntry(t, out, "gated", own)
	require.NotContains(t, ownEntry, "body", "held work is listed without its body")
	require.Equal(t, "sitting:security", ownEntry["gate"].(map[string]any)["who"])
	behindEntry := groupEntry(t, out, "gated", behind)
	blockers := behindEntry["blockers"].([]any)
	require.Len(t, blockers, 1)
	require.Equal(t, "gate", blockers[0].(map[string]any)["kind"])
	require.Equal(t, "owner", blockers[0].(map[string]any)["gate"].(map[string]any)["who"], "a gate blocker says who it waits on")

	// The agent's queue and the owner's queue.
	agent := callJSON(t, ctx, cli, "tasks_ready", map[string]any{"agent": true})
	require.Equal(t, []string{"project", "ready", "omitted"}, keysOf(agent))
	require.Equal(t, map[string]any{"blocked": float64(0), "gated": float64(2), "gates": float64(1)}, agent["omitted"])
	owner := callJSON(t, ctx, cli, "tasks_ready", map[string]any{"who": "OWNER"})
	require.Equal(t, []string{gateID}, groupIDs(t, owner, "gates"))
	require.Equal(t, []string{behind}, groupIDs(t, owner, "gated"), "a step behind the owner's gate waits on the owner")
	require.NotContains(t, owner, "ready")
	sitting := callJSON(t, ctx, cli, "tasks_ready", map[string]any{"who": "sitting"})
	require.Empty(t, groupIDs(t, sitting, "gates"))
	require.Equal(t, []string{own}, groupIDs(t, sitting, "gated"), "a class matches every name in it")
	for _, bad := range []map[string]any{{"who": "  "}, {"who": "owner", "agent": true}} {
		isErr, _ := callErr(t, ctx, cli, "tasks_ready", bad)
		require.True(t, isErr, "%v", bad)
	}

	// Claims: a gate is never claimed; a step with an open gate waits for it.
	isErr, txt := callErr(t, ctx, cli, "tasks_claim", map[string]any{"id": gateID})
	require.True(t, isErr)
	require.Contains(t, txt, "task gated")
	isErr, txt = callErr(t, ctx, cli, "tasks_claim", map[string]any{"id": own})
	require.True(t, isErr)
	require.Contains(t, txt, "sitting:security: the sitting runs the drill")

	// Clearing a step's gate makes it ready and leaves its status.
	cleared := callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": own, "gate_clear": true, "gate_note": "ran 2026-11-14"})
	require.Equal(t, "open", cleared["status"])
	g := cleared["gate"].(map[string]any)
	require.Equal(t, false, g["open"])
	require.Equal(t, "ran 2026-11-14", g["note"])
	require.NotEmpty(t, g["cleared_at"])
	require.NotEmpty(t, g["cleared_by"], "the acting session cleared it")
	out = callJSON(t, ctx, cli, "tasks_ready", nil)
	require.ElementsMatch(t, []string{free, own}, readyIDs(t, out))
	hist := transitions(t, db, own)
	require.Equal(t, "cleared", hist[len(hist)-1].payload["gate"])
	require.Equal(t, "set", hist[0].payload["gate"])

	// Clearing the gate task closes it and frees what it held.
	closed := callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": gateID, "gate_clear": true})
	require.Equal(t, "done", closed["status"])
	out = callJSON(t, ctx, cli, "tasks_ready", nil)
	require.ElementsMatch(t, []string{free, own, behind}, readyIDs(t, out))
	require.NotContains(t, out, "gates")
	require.NotContains(t, out, "gated")

	// The lookup shows the gate.
	listed := callJSON(t, ctx, cli, "tasks_list", map[string]any{"id": gateID})["tasks"].([]any)[0].(map[string]any)
	require.Equal(t, "gate", listed["kind"])
	require.Equal(t, false, listed["gate"].(map[string]any)["open"])
}

func TestTasksGates_UpdateArguments(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	step := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "GATE (founder): pick a name"})["id"].(string)

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"a gate kind without a gate", map[string]any{"kind": "gate"}},
		{"an unknown kind", map[string]any{"kind": "milestone"}},
		{"who without what", map[string]any{"gate_who": "owner"}},
		{"a note without a clear", map[string]any{"gate_note": "why"}},
		{"nothing to clear", map[string]any{"gate_clear": true}},
		{"a clear that is false", map[string]any{"gate_clear": false}},
		{"a two-line what", map[string]any{"gate_who": "owner", "gate_what": "one\ntwo"}},
	} {
		args := map[string]any{"id": step}
		for k, v := range tc.args {
			args[k] = v
		}
		isErr, txt := callErr(t, ctx, cli, "tasks_update", args)
		require.True(t, isErr, "%s: %s", tc.name, txt)
	}

	// The GATE-task workaround converts in one call.
	out := callJSON(t, ctx, cli, "tasks_update", map[string]any{
		"id": step, "kind": "gate", "gate_who": "founder:dana", "gate_what": "pick the name",
	})
	require.Equal(t, "gate", out["kind"])
	hist := transitions(t, db, step)
	last := hist[len(hist)-1].payload
	require.Equal(t, "gate", last["kind"])
	require.Equal(t, "set", last["gate"])

	isErr, txt := callErr(t, ctx, cli, "tasks_update", map[string]any{"id": step, "gate_what": "x", "gate_clear": true})
	require.True(t, isErr, txt)

	// Emptying who and what removes a step's gate without clearing it.
	stepAgain := core.TaskKindStep
	out = callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": step, "kind": string(stepAgain), "gate_who": "", "gate_what": ""})
	require.NotContains(t, out, "gate")
	require.NotContains(t, out, "kind")
	hist = transitions(t, db, step)
	require.Equal(t, "removed", hist[len(hist)-1].payload["gate"])
	task, err := store.TaskByID(ctx, db, step)
	require.NoError(t, err)
	require.False(t, task.Gate.Set())
}

// A caller outside a confidential project may claim its tasks, but the
// gated refusal names no one there: who and what are the project's content.
func TestTasksGates_ConfidentialClaimRefusalNamesNoOne(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	isolate(t, ctx, db, "client-work", core.IsolationConfidential)
	inside := bindClient(t, ctx, db, url, "client-work")
	taskID := callJSON(t, ctx, inside, "tasks_add", map[string]any{
		"title": "rotate the keys", "gate_who": "founder:dana", "gate_what": "approve the rotation window",
	})["id"].(string)

	isErr, txt := callErr(t, ctx, inside, "tasks_claim", map[string]any{"id": taskID})
	require.True(t, isErr)
	require.Contains(t, txt, "founder:dana", "inside, the refusal says who")

	outside := bindClient(t, ctx, db, url, "demo")
	isErr, txt = callErr(t, ctx, outside, "tasks_claim", map[string]any{"id": taskID})
	require.True(t, isErr)
	require.Contains(t, txt, "task gated")
	require.NotContains(t, txt, "founder")
	require.NotContains(t, txt, "rotation")

	out := callJSON(t, ctx, outside, "tasks_update", map[string]any{"id": taskID, "gate_clear": true})
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "founder", "a withheld answer carries no gate")
	require.NotContains(t, out, "kind")
}

// keysOf returns a tasks_ready answer's keys in the order the tests expect:
// project first, then the groups in the order the tool documents them.
func keysOf(out map[string]any) []string {
	var keys []string
	for _, k := range []string{"project", "who", "ready", "blocked", "gated", "gates", "omitted"} {
		if _, ok := out[k]; ok {
			keys = append(keys, k)
		}
	}
	if len(keys) != len(out) {
		keys = append(keys, "<unexpected keys>")
	}
	return keys
}
