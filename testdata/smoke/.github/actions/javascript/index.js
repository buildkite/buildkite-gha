const fs = require("node:fs");

const message = process.env.INPUT_MESSAGE;
if (!message) {
  throw new Error("message input is required");
}

fs.appendFileSync(process.env.GITHUB_OUTPUT, `result=${message}-javascript\n`);
fs.appendFileSync(process.env.GITHUB_STATE, "phase=main\n");
fs.appendFileSync(
  process.env.GITHUB_STEP_SUMMARY,
  "JavaScript smoke action completed its main phase.\n",
);

console.log("::add-mask::smoke-mask-value");
console.log("masked probe: smoke-mask-value");
