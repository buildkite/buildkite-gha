const childProcess = require("node:child_process");
const fs = require("node:fs");

if (process.getuid() === 0 || childProcess.execFileSync("id", ["-un"], { encoding: "utf8" }).trim() !== "runner") {
  throw new Error("JavaScript post action did not run as the non-root runner user");
}
if (process.env.HOME !== "/home/runner" || process.env.STATE_phase !== "main") {
  throw new Error("JavaScript post action did not retain HOME and action state");
}
fs.appendFileSync(process.env.GITHUB_ENV, "PB2731_JAVASCRIPT_POST=true\n");
fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, "JavaScript post cleanup ran as `runner`.\n");
console.log("PB2731_JAVASCRIPT_POST=runner");
