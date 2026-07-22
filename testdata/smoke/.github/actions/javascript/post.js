const fs = require("node:fs");

if (process.env.STATE_phase !== "main") {
  throw new Error(`unexpected action state: ${process.env.STATE_phase || "missing"}`);
}

fs.appendFileSync(
  process.env.GITHUB_STEP_SUMMARY,
  "JavaScript smoke action completed its post phase.\n",
);
console.log("::notice title=Smoke action post::JavaScript post phase completed");
