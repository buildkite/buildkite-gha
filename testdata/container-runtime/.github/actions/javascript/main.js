const fs = require("node:fs");

const message = process.env.INPUT_MESSAGE;
if (!message) {
  throw new Error("message input is required");
}

fs.appendFileSync(process.env.GITHUB_OUTPUT, `result=${message}-javascript\n`);
fs.appendFileSync(process.env.GITHUB_ENV, "CONTAINER_RUNTIME_JAVASCRIPT_ENV=seen\n");
fs.appendFileSync(process.env.GITHUB_STATE, "phase=main\n");
fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, "container runtime JavaScript main\n");
console.log("::add-mask::container-runtime-javascript-secret");
console.log("masked JavaScript probe: container-runtime-javascript-secret");
