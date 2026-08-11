const fs = require("node:fs");

if (process.env.STATE_phase !== "main") {
  throw new Error(`unexpected action state: ${process.env.STATE_phase || "missing"}`);
}

fs.appendFileSync(
  process.env.GITHUB_STEP_SUMMARY,
  "JavaScript oracle action completed its post phase.\n",
);
console.log("::notice title=Oracle action post::JavaScript post phase completed");
