const childProcess = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

if (process.getuid() === 0 || childProcess.execFileSync("id", ["-un"], { encoding: "utf8" }).trim() !== "runner") {
  throw new Error("JavaScript action did not run as the non-root runner user");
}
if (process.env.HOME !== "/home/runner") {
  throw new Error(`unexpected HOME: ${process.env.HOME}`);
}
for (const directory of [process.env.GITHUB_WORKSPACE, process.env.RUNNER_TEMP, process.env.RUNNER_TOOL_CACHE]) {
  fs.accessSync(directory, fs.constants.W_OK);
  const probe = path.join(directory, `.pb2731-javascript-${process.pid}`);
  fs.writeFileSync(probe, "runner\n");
  fs.unlinkSync(probe);
}
for (const name of ["GITHUB_ENV", "GITHUB_OUTPUT", "GITHUB_PATH", "GITHUB_STATE", "GITHUB_STEP_SUMMARY"]) {
  fs.accessSync(process.env[name], fs.constants.W_OK);
}

const bin = path.join(process.env.RUNNER_TEMP, "pb2731-javascript-bin");
fs.mkdirSync(bin, { recursive: true });
fs.appendFileSync(process.env.GITHUB_ENV, "PB2731_JAVASCRIPT_MAIN=true\n");
fs.appendFileSync(process.env.GITHUB_OUTPUT, "identity=runner\n");
fs.appendFileSync(process.env.GITHUB_PATH, `${bin}\n`);
fs.appendFileSync(process.env.GITHUB_STATE, "phase=main\n");
fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, "JavaScript action ran as `runner`.\n");
console.log("PB2731_JAVASCRIPT_MAIN=runner");
