const fs = require("node:fs");

if (process.env.STATE_phase !== "main") {
  throw new Error(`unexpected phase state: ${process.env.STATE_phase || "missing"}`);
}
fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, `runtime post ${process.env.INPUT_ORDER || "single"}\n`);
console.log(`lifecycle:post:${process.env.INPUT_ORDER || "single"}`);
