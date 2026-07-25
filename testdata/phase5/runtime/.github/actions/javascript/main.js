const fs = require("node:fs");

const message = process.env.INPUT_MESSAGE;
if (!message) {
  throw new Error("message input is required");
}

fs.appendFileSync(process.env.GITHUB_OUTPUT, `result=${message}-javascript\n`);
fs.appendFileSync(process.env.GITHUB_ENV, "PHASE5_JAVASCRIPT_ENV=seen\n");
fs.appendFileSync(process.env.GITHUB_STATE, "phase=main\n");
fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, "phase5 JavaScript main\n");
console.log("::add-mask::phase5-javascript-secret");
console.log("masked JavaScript probe: phase5-javascript-secret");
