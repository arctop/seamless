---
id: 01M6TA9WGPBRGWSGJKD3PB3HR6
kind: convention
name: american-spelling-enroll
description: Seamless spells enroll/enrollment the American way everywhere (code, identifiers, UI, docs, wire strings); never enrol/enrolment
project: seamless
created: 2026-11-07T06:14:31Z
invalid_at: null
superseded_by: null
source_session: 01M6T7RADK20APPACCVDP20K47
model: claude-opus-5-5
tags: [spelling, copy, plan:seamless-sealed-unlock]
---
Owner, 2026-11-06 (session cc/e4c94d5a): use the American "enroll" everywhere: enroll, enrolls, enrollment, enrolled, enrolling. Never the British "enrol", "enrols", "enrolment".

This covers identifiers (TouchIDEnrollMessage, approver_enroll_codes), user-facing copy (the Seamless Approve dialog, CLI output, console), docs, and Seamless notes or drafts that become docs.

Commit 6cf90a4 on branch locked-projects converted all 410 occurrences. It changed the wire strings on both sides at once, because nothing had shipped: the route POST /console/approvers/touchid/enroll, the signed tags "seamless touchid enrollment v1" and "seamless approver enrollment v1", and the table approver_enroll_codes (migration 032 edited in place).

Check for regressions with `git grep -nP '[Ee]nrol(?!l)'`, which must find nothing outside history. Fix in bulk with `perl -pi -e 's/([Ee])nrol(?!l)/$1nroll/g'`. "Enrolled" and "enrolling" are the same in both spellings, which is why the regex leaves a following "l" alone.
