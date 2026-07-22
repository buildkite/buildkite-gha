const fs = require("node:fs");

fs.appendFileSync(process.env.GITHUB_OUTPUT, `result=${process.env.INPUT_MESSAGE}-javascript\n`);
fs.appendFileSync(process.env.GITHUB_ENV, "RUNTIME_SEEN=true\n");
fs.appendFileSync(process.env.GITHUB_STATE, "phase=main\n");
fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, "runtime main summary\n");
console.log("::add-mask::runtime-secret-value");
console.log("masked probe: runtime-secret-value");
console.log("lifecycle:main");

if (process.env.INPUT_FAIL === "true") {
  throw new Error("requested main failure");
}
