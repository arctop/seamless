package retrieve

// The defaults row's expected output: every surface of acctFixture(t, false)
// rendered with no View, captured from the tree as it stood BEFORE account
// labels existed (the parent of the change that added them). It is literal on
// purpose, like cmd/seam's golden_test.go: a renderer change has to edit this
// file, in the diff, rather than slip through. Ages render "1h" because the
// fixture dates every item 80-90 minutes back.

var acctGolden = acctSurfaceSet{
	Briefing: `<seam-briefing>
Seam project: app -- 10 memories (3 constraints, 1 convention, 1 stage), 3 recent findings.

Constraints (binding for every session):
- app-rule-one: zephyr rule one keeps the build green
- app-rule-two: zephyr rule two binds every session
- +1 more, equally binding -- memory_read name=<name> before working near one: app-rule-three

Stages (world-state gates):
- app-stage -- open, gate human
(details: memory_read name=<stage>)

Plans:
- app-ship -- 1/2 done, 1 claimable, 0 in flight
- app-ship -- awaiting approval: Ship zephyr (presented, 1h)
(steps: tasks_ready plan=<slug>; claim: tasks_claim id=<task id> session=<your Seam session>; attach work via the plan:<slug> tag)

Conventions (project-local choices):
- app-conv: zephyr indent with tabs
(1 total -- recall kind=convention lists them)

Recent findings:
- cc/global-one (1h): zephyr global finding
- cc/app-one (1h): zephyr app finding by the account
- cc/app-two (1h): zephyr app finding by the owner

Ready tasks (2):
- zephyr owner task
- zephyr ready task
(full queue with ids: tasks_ready; claim: tasks_claim id=<id> session=<your Seam session>)

Memories (app):
- app-star: zephyr starred gotcha
- parent-fact: zephyr parent knowledge
- global-fact: zephyr applies everywhere
- app-gotcha: zephyr cache race during boot
- app-ref: zephyr reference doc

Sibling projects:
- sib (1h): zephyr sibling finding
- ghost-proj (1h): zephyr ghost finding
- locked-fin (1h): zephyr locked finding

Sibling memories:
- ghost-proj/ghost-secret-conv: zephyr ghost knowledge convention
- ghost-proj/ghost-secret-fact: zephyr ghost knowledge
- locked-fin/fin-secret-conv: zephyr locked knowledge convention
- locked-fin/fin-secret-fact: zephyr locked knowledge
- sib/sib-fact: zephyr sibling knowledge
Recall on demand with recall; read a memory with memory_read. Trust a tool's inputSchema (required + enums) over prose when building a call.
</seam-briefing>`,
	Resumed: `<seam-briefing>
Seam project: app -- 10 memories (3 constraints, 1 convention, 1 stage), 3 recent findings.

Constraints (binding for every session):
- app-rule-one: zephyr rule one keeps the build green
- app-rule-two: zephyr rule two binds every session
- +1 more, equally binding -- memory_read name=<name> before working near one: app-rule-three

Stages (world-state gates):
- app-stage -- open, gate human
(details: memory_read name=<stage>)

Plans:
- app-ship -- 1/2 done, 1 claimable, 0 in flight
- app-ship -- awaiting approval: Ship zephyr (presented, 1h)
(steps: tasks_ready plan=<slug>; claim: tasks_claim id=<task id> session=<your Seam session>; attach work via the plan:<slug> tag)

Conventions (project-local choices):
- app-conv: zephyr indent with tabs
(1 total -- recall kind=convention lists them)

Recent findings:
- cc/global-one (1h): zephyr global finding
- cc/app-one (1h): zephyr app finding by the account
- cc/app-two (1h): zephyr app finding by the owner

Ready tasks (2):
- zephyr owner task
- zephyr ready task
(full queue with ids: tasks_ready; claim: tasks_claim id=<id> session=<your Seam session>)

Memories (app):
- app-star: zephyr starred gotcha
- parent-fact: zephyr parent knowledge
- global-fact: zephyr applies everywhere
- app-gotcha: zephyr cache race during boot
- app-ref: zephyr reference doc

Sibling projects:
- sib (1h): zephyr sibling finding
- ghost-proj (1h): zephyr ghost finding
- locked-fin (1h): zephyr locked finding

Sibling memories:
- ghost-proj/ghost-secret-conv: zephyr ghost knowledge convention
- ghost-proj/ghost-secret-fact: zephyr ghost knowledge
- locked-fin/fin-secret-conv: zephyr locked knowledge convention
- locked-fin/fin-secret-fact: zephyr locked knowledge
- sib/sib-fact: zephyr sibling knowledge
Recall on demand with recall; read a memory with memory_read. Trust a tool's inputSchema (required + enums) over prose when building a call.
(resumed session -- earlier context may be summarized; recall to re-ground.)
</seam-briefing>`,
	Subagent: `<seam-briefing>
Seam project: app -- 3 constraints (subagent scope).

Constraints (binding for every session):
- app-rule-one: zephyr rule one keeps the build green
- app-rule-two: zephyr rule two binds every session
- +1 more, equally binding -- memory_read name=<name> before working near one: app-rule-three

Relevant to this task:
- app-gotcha: zephyr cache race during boot
Recall on demand with recall; read a memory with memory_read.
</seam-briefing>`,
	Prompt: `<seam-recall>Seam has possibly relevant memories:
- app-gotcha (1h): zephyr cache race during boot
Read with memory_read before re-deriving.</seam-recall>`,
	BriefingIDs: []string{"01AC1", "01AC2", "01AC3", "01AST", "01ACV", "01AFV", "01PAR", "01G01", "01AGT", "01ARF", "01Xghost-secret-conv", "01Xghost-secret-fact", "01Xfin-secret-conv", "01Xfin-secret-fact", "01SIB"},
	SubagentIDs: []string{"01AC1", "01AC2", "01AC3", "01AGT"},
	PromptIDs:   []string{"01AGT"},
	Recall: `[` +
		`{"kind":"memory","id":"01AFV","name":"app-star","title":"app-star","description":"zephyr starred gotcha","project":"app","age":"1h","source":"fts","score":0.017424242424242422,"favorite":true},` +
		`{"kind":"trial","id":"01TRL","name":"app-lab","title":"zephyr sweep","description":"zephyr settled","project":"app","age":"1h","source":"fts","score":0.01639344262295082,"status":"pass"},` +
		`{"kind":"note","id":"01NPL","name":"cc-plan-ship","title":"Ship zephyr","description":"zephyr ship plan","project":"app","age":"1h","source":"fts","score":0.016129032258064516},` +
		`{"kind":"task","id":"01TRD","name":"","title":"zephyr ready task","description":"zephyr ready task body","project":"app","age":"1h","source":"fts","score":0.015873015873015872,"status":"open"},` +
		`{"kind":"task","id":"01TRE","name":"","title":"zephyr owner task","description":"zephyr owner task body","project":"app","age":"1h","source":"fts","score":0.015625,"status":"open"},` +
		`{"kind":"note","id":"01NAP","name":"app-note","title":"zephyr app note","description":"zephyr app note body","project":"app","age":"1h","source":"fts","score":0.015384615384615385},` +
		`{"kind":"memory","id":"01ARF","name":"app-ref","title":"app-ref","description":"zephyr reference doc","project":"app","age":"1h","source":"fts","score":0.014925373134328358},` +
		`{"kind":"memory","id":"01AST","name":"app-stage","title":"app-stage","description":"zephyr deploy gate","project":"app","age":"1h","source":"fts","score":0.014705882352941176},` +
		`{"kind":"memory","id":"01G01","name":"global-fact","title":"global-fact","description":"zephyr applies everywhere","age":"1h","source":"fts","score":0.014492753623188406},` +
		`{"kind":"memory","id":"01ACV","name":"app-conv","title":"app-conv","description":"zephyr indent with tabs","project":"app","age":"1h","source":"fts","score":0.014285714285714285},` +
		`{"kind":"memory","id":"01AGT","name":"app-gotcha","title":"app-gotcha","description":"zephyr cache race during boot","project":"app","age":"1h","source":"fts","score":0.014084507042253521},` +
		`{"kind":"memory","id":"01AC2","name":"app-rule-two","title":"app-rule-two","description":"zephyr rule two binds every session","project":"app","age":"1h","source":"fts","score":0.013888888888888888},` +
		`{"kind":"memory","id":"01AC3","name":"app-rule-three","title":"app-rule-three","description":"zephyr rule three is the oldest","project":"app","age":"1h","source":"fts","score":0.0136986301369863},` +
		`{"kind":"session","id":"01SGL","name":"cc/global-one","title":"cc/global-one","description":"zephyr global finding","age":"1h","source":"fts","score":0.013513513513513514,"status":"completed"},` +
		`{"kind":"task","id":"01TP1","name":"app-ship","title":"zephyr ship step one","description":"","project":"app","age":"1h","source":"fts","score":0.013333333333333334,"status":"done"},` +
		`{"kind":"task","id":"01TP2","name":"app-ship","title":"zephyr ship step two","description":"","project":"app","age":"1h","source":"fts","score":0.013157894736842105,"status":"open"},` +
		`{"kind":"memory","id":"01AC1","name":"app-rule-one","title":"app-rule-one","description":"zephyr rule one keeps the build green","project":"app","age":"1h","source":"fts","score":0.012987012987012988},` +
		`{"kind":"session","id":"01SA2","name":"cc/app-two","title":"cc/app-two","description":"zephyr app finding by the owner","project":"app","age":"1h","source":"fts","score":0.01282051282051282,"status":"completed"},` +
		`{"kind":"session","id":"01SAP","name":"cc/app-one","title":"cc/app-one","description":"zephyr app finding by the account","project":"app","age":"1h","source":"fts","score":0.012658227848101266,"status":"completed"}` +
		`]`,
	RecallTasks: `[` +
		`{"kind":"task","id":"01TRD","name":"","title":"zephyr ready task","description":"zephyr ready task body","project":"app","age":"1h","source":"fts","score":0.01639344262295082,"status":"open"},` +
		`{"kind":"task","id":"01TRE","name":"","title":"zephyr owner task","description":"zephyr owner task body","project":"app","age":"1h","source":"fts","score":0.016129032258064516,"status":"open"},` +
		`{"kind":"task","id":"01TP1","name":"app-ship","title":"zephyr ship step one","description":"","project":"app","age":"1h","source":"fts","score":0.015873015873015872,"status":"done"},` +
		`{"kind":"task","id":"01TP2","name":"app-ship","title":"zephyr ship step two","description":"","project":"app","age":"1h","source":"fts","score":0.015625,"status":"open"}` +
		`]`,
	Browse: `[` +
		`{"kind":"memory","id":"01AC1","name":"app-rule-one","title":"app-rule-one","description":"zephyr rule one keeps the build green","project":"app","age":"1h","source":"browse","score":0},` +
		`{"kind":"memory","id":"01AC2","name":"app-rule-two","title":"app-rule-two","description":"zephyr rule two binds every session","project":"app","age":"1h","source":"browse","score":0},` +
		`{"kind":"memory","id":"01AC3","name":"app-rule-three","title":"app-rule-three","description":"zephyr rule three is the oldest","project":"app","age":"1h","source":"browse","score":0}` +
		`]`,
	Search: `[` +
		`{"kind":"note","id":"01NPL","name":"cc-plan-ship","title":"Ship zephyr","description":"zephyr ship plan","project":"app","age":"1h","source":"fts","score":0.01639344262295082,"snippet":"Ship \u0001zephyr\u0002"},` +
		`{"kind":"note","id":"01NAP","name":"app-note","title":"zephyr app note","description":"zephyr app note body","project":"app","age":"1h","source":"fts","score":0.016129032258064516,"snippet":"\u0001zephyr\u0002 app note"},` +
		`{"kind":"note","id":"01Xconf-secret-note","name":"conf-secret-note","title":"zephyr confidential knowledge note","description":"zephyr confidential knowledge","project":"conf-hq","age":"1h","source":"fts","score":0.015873015873015872,"snippet":"\u0001zephyr\u0002 confidential knowledge note"},` +
		`{"kind":"note","id":"01Xfin-secret-note","name":"fin-secret-note","title":"zephyr locked knowledge note","description":"zephyr locked knowledge","project":"locked-fin","age":"1h","source":"fts","score":0.015625,"snippet":"\u0001zephyr\u0002 locked knowledge note"},` +
		`{"kind":"note","id":"01Xghost-secret-note","name":"ghost-secret-note","title":"zephyr ghost knowledge note","description":"zephyr ghost knowledge","project":"ghost-proj","age":"1h","source":"fts","score":0.015384615384615385,"snippet":"\u0001zephyr\u0002 ghost knowledge note"},` +
		`{"kind":"note","id":"01Xvault-secret-note","name":"vault-secret-note","title":"zephyr sealed knowledge note","description":"zephyr sealed knowledge","project":"sealed-vault","age":"1h","source":"fts","score":0.015151515151515152,"snippet":"\u0001zephyr\u0002 sealed knowledge note"},` +
		`{"kind":"memory","id":"01AFV","name":"app-star","title":"app-star","description":"zephyr starred gotcha","project":"app","age":"1h","source":"fts","score":0.014925373134328358,"snippet":"\u0001zephyr\u0002 starred gotcha","favorite":true},` +
		`{"kind":"memory","id":"01ARF","name":"app-ref","title":"app-ref","description":"zephyr reference doc","project":"app","age":"1h","source":"fts","score":0.014705882352941176,"snippet":"\u0001zephyr\u0002 reference doc"},` +
		`{"kind":"memory","id":"01AST","name":"app-stage","title":"app-stage","description":"zephyr deploy gate","project":"app","age":"1h","source":"fts","score":0.014492753623188406,"snippet":"\u0001zephyr\u0002 deploy gate"},` +
		`{"kind":"memory","id":"01G01","name":"global-fact","title":"global-fact","description":"zephyr applies everywhere","age":"1h","source":"fts","score":0.014285714285714285,"snippet":"\u0001zephyr\u0002 applies everywhere"},` +
		`{"kind":"memory","id":"01PAR","name":"parent-fact","title":"parent-fact","description":"zephyr parent knowledge","project":"app-parent","age":"1h","source":"fts","score":0.014084507042253521,"snippet":"\u0001zephyr\u0002 parent knowledge"},` +
		`{"kind":"memory","id":"01SIB","name":"sib-fact","title":"sib-fact","description":"zephyr sibling knowledge","project":"sib","age":"1h","source":"fts","score":0.013888888888888888,"snippet":"\u0001zephyr\u0002 sibling knowledge"},` +
		`{"kind":"memory","id":"01Xconf-secret-fact","name":"conf-secret-fact","title":"conf-secret-fact","description":"zephyr confidential knowledge","project":"conf-hq","age":"1h","source":"fts","score":0.0136986301369863,"snippet":"\u0001zephyr\u0002 confidential knowledge"},` +
		`{"kind":"memory","id":"01Xfin-secret-fact","name":"fin-secret-fact","title":"fin-secret-fact","description":"zephyr locked knowledge","project":"locked-fin","age":"1h","source":"fts","score":0.013513513513513514,"snippet":"\u0001zephyr\u0002 locked knowledge"},` +
		`{"kind":"memory","id":"01Xghost-secret-fact","name":"ghost-secret-fact","title":"ghost-secret-fact","description":"zephyr ghost knowledge","project":"ghost-proj","age":"1h","source":"fts","score":0.013333333333333334,"snippet":"\u0001zephyr\u0002 ghost knowledge"},` +
		`{"kind":"memory","id":"01Xvault-secret-fact","name":"vault-secret-fact","title":"vault-secret-fact","description":"zephyr sealed knowledge","project":"sealed-vault","age":"1h","source":"fts","score":0.013157894736842105,"snippet":"\u0001zephyr\u0002 sealed knowledge"},` +
		`{"kind":"memory","id":"01ACV","name":"app-conv","title":"app-conv","description":"zephyr indent with tabs","project":"app","age":"1h","source":"fts","score":0.012987012987012988,"snippet":"\u0001zephyr\u0002 indent with tabs"},` +
		`{"kind":"memory","id":"01Xconf-secret-conv","name":"conf-secret-conv","title":"conf-secret-conv","description":"zephyr confidential knowledge convention","project":"conf-hq","age":"1h","source":"fts","score":0.01282051282051282,"snippet":"\u0001zephyr\u0002 confidential knowledge convention"},` +
		`{"kind":"memory","id":"01Xconf-secret-rule","name":"conf-secret-rule","title":"conf-secret-rule","description":"zephyr confidential knowledge rule","project":"conf-hq","age":"1h","source":"fts","score":0.012658227848101266,"snippet":"\u0001zephyr\u0002 confidential knowledge rule"},` +
		`{"kind":"memory","id":"01Xfin-secret-conv","name":"fin-secret-conv","title":"fin-secret-conv","description":"zephyr locked knowledge convention","project":"locked-fin","age":"1h","source":"fts","score":0.0125,"snippet":"\u0001zephyr\u0002 locked knowledge convention"},` +
		`{"kind":"memory","id":"01Xfin-secret-rule","name":"fin-secret-rule","title":"fin-secret-rule","description":"zephyr locked knowledge rule","project":"locked-fin","age":"1h","source":"fts","score":0.012345679012345678,"snippet":"\u0001zephyr\u0002 locked knowledge rule"},` +
		`{"kind":"memory","id":"01Xghost-secret-conv","name":"ghost-secret-conv","title":"ghost-secret-conv","description":"zephyr ghost knowledge convention","project":"ghost-proj","age":"1h","source":"fts","score":0.012195121951219513,"snippet":"\u0001zephyr\u0002 ghost knowledge convention"},` +
		`{"kind":"memory","id":"01Xghost-secret-rule","name":"ghost-secret-rule","title":"ghost-secret-rule","description":"zephyr ghost knowledge rule","project":"ghost-proj","age":"1h","source":"fts","score":0.012048192771084338,"snippet":"\u0001zephyr\u0002 ghost knowledge rule"},` +
		`{"kind":"memory","id":"01Xvault-secret-conv","name":"vault-secret-conv","title":"vault-secret-conv","description":"zephyr sealed knowledge convention","project":"sealed-vault","age":"1h","source":"fts","score":0.011904761904761904,"snippet":"\u0001zephyr\u0002 sealed knowledge convention"},` +
		`{"kind":"memory","id":"01Xvault-secret-rule","name":"vault-secret-rule","title":"vault-secret-rule","description":"zephyr sealed knowledge rule","project":"sealed-vault","age":"1h","source":"fts","score":0.011764705882352941,"snippet":"\u0001zephyr\u0002 sealed knowledge rule"},` +
		`{"kind":"memory","id":"01AGT","name":"app-gotcha","title":"app-gotcha","description":"zephyr cache race during boot","project":"app","age":"1h","source":"fts","score":0.011627906976744186,"snippet":"\u0001zephyr\u0002 cache race during boot"},` +
		`{"kind":"memory","id":"01AC2","name":"app-rule-two","title":"app-rule-two","description":"zephyr rule two binds every session","project":"app","age":"1h","source":"fts","score":0.011494252873563218,"snippet":"\u0001zephyr\u0002 rule two binds every session"},` +
		`{"kind":"memory","id":"01AC3","name":"app-rule-three","title":"app-rule-three","description":"zephyr rule three is the oldest","project":"app","age":"1h","source":"fts","score":0.011363636363636364,"snippet":"\u0001zephyr\u0002 rule three is the oldest"},` +
		`{"kind":"memory","id":"01AC1","name":"app-rule-one","title":"app-rule-one","description":"zephyr rule one keeps the build green","project":"app","age":"1h","source":"fts","score":0.011235955056179775,"snippet":"\u0001zephyr\u0002 rule one keeps the build green"}` +
		`]`,
}
