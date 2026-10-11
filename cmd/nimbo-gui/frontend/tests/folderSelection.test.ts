// Rules of the folder picker's tickboxes and staged changes. Runs on Node's own
// test runner (Node strips the types itself): `npm test` in the frontend folder.
// Kept outside src/ so svelte-check doesn't need Node's type definitions.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  tickState, toggle, planChanges, planEmpty, asksKeepOrDelete, freedBytes, downloadBytes, rowChange, fmtSize,
} from "../src/folderSelection.ts";

test("tick state follows the excludes", () => {
  const ex = ["Photos", "Work/Old"];
  assert.equal(tickState("Photos", ex), "off");
  assert.equal(tickState("Photos/2024", ex), "inherited");
  assert.equal(tickState("Work", ex), "partial");
  assert.equal(tickState("Work/New", ex), "on");
  assert.equal(tickState("PhotosBackup", ex), "on"); // a name that merely starts the same
  assert.equal(tickState("Work/Old", ex), "off");
});

test("off then on again is no change at all", () => {
  const committed = ["Work/Old"];
  let t = toggle("Work", committed, committed); // partial → on (sync all of it)
  assert.deepEqual(t, []);
  t = toggle("Work", t, committed); // on → off: the saved exclude inside comes back
  assert.equal(tickState("Work", t), "off");
  t = toggle("Work", t, committed); // off → partial again, exactly as saved
  assert.deepEqual(t, committed);
  assert.ok(planEmpty(planChanges(t, committed)));
});

test("ticking a half-ticked folder syncs all of it", () => {
  const committed = ["Work/Old", "Work/Tmp/Cache"];
  const t = toggle("Work", committed, committed);
  assert.deepEqual(t, []);
  assert.deepEqual(planChanges(t, committed), { exclude: [], include: ["Work/Old", "Work/Tmp/Cache"] });
});

test("unticking a parent drops what was staged inside it", () => {
  const committed: string[] = [];
  let t = toggle("Work/A", committed, committed); // stage A off
  t = toggle("Work/B", t, committed); // stage B off
  assert.equal(tickState("Work", t), "partial");
  t = toggle("Work", t, committed); // partial → on (clears A and B)
  t = toggle("Work", t, committed); // on → off
  assert.deepEqual(t, ["Work"]);
  assert.deepEqual(planChanges(t, committed), { exclude: ["Work"], include: [] });
});

test("unticking a parent whose child was staged back on sends no call for the child", () => {
  const committed = ["Work/Old"];
  let t = toggle("Work/Old", committed, committed); // stage Old back on
  assert.deepEqual(planChanges(t, committed), { exclude: [], include: ["Work/Old"] });
  t = toggle("Work", t, committed); // untick Work: Old's saved exclude returns
  assert.deepEqual(planChanges(t, committed), { exclude: ["Work"], include: [] });
});

test("inside an unticked folder nothing can be changed", () => {
  const committed = ["Work"];
  assert.deepEqual(toggle("Work/A", committed, committed), committed);
});

test("re-include a folder and untick one inside it in the same go", () => {
  const committed = ["Work"];
  let t = toggle("Work", committed, committed); // Work on again
  t = toggle("Work/Big", t, committed); // but not Big
  const plan = planChanges(t, committed);
  assert.deepEqual(plan, { exclude: ["Work/Big"], include: ["Work"] });
  // Big was never on this PC (Work wasn't syncing), so there's nothing to keep or delete.
  assert.equal(asksKeepOrDelete(plan, committed), false);
  assert.equal(asksKeepOrDelete(planChanges(["Photos"], []), []), true);
});

test("a plan never excludes a folder inside another it already excludes", () => {
  // Can't be built by clicking, but a hand-made target must still be safe.
  const plan = planChanges(["A", "A/B", "C"], []);
  assert.deepEqual(plan.exclude, ["A", "C"]);
});

test("space freed and to download", () => {
  const sizes = { Work: 1000, "Work/Old": 300, "Work/Old/Deep": 100, Photos: 500 };
  const committed = ["Work/Old"];
  let t = toggle("Work", committed, committed); // partial → on
  t = toggle("Work", t, committed); // → off
  t = toggle("Photos", t, committed);
  const plan = planChanges(t, committed);
  // Work frees 1000 less the 300 that was never here; Photos 500.
  assert.equal(freedBytes(plan, committed, sizes), 1200);
  // Re-including Old (and nothing else) downloads 300.
  const t2 = toggle("Work/Old", committed, committed);
  assert.equal(downloadBytes(planChanges(t2, committed), t2, sizes), 300);
  // Unknown sizes count as nothing rather than NaN.
  assert.equal(freedBytes({ exclude: ["Nope"], include: [] }, [], sizes), 0);
});

test("row labels", () => {
  const plan = { exclude: ["Work/A"], include: ["Old"] };
  assert.equal(rowChange("Work/A", plan), "stops");
  assert.equal(rowChange("Old", plan), "starts");
  assert.equal(rowChange("Work", plan), "inside");
  assert.equal(rowChange("Photos", plan), "");
});

test("sizes read short", () => {
  assert.equal(fmtSize(0), "0 B");
  assert.equal(fmtSize(512), "512 B");
  assert.equal(fmtSize(1536), "1.5 KB");
  assert.equal(fmtSize(5 * 1024 * 1024 * 1024), "5.0 GB");
  assert.equal(fmtSize(12 * 1024 * 1024 * 1024), "12 GB");
});
