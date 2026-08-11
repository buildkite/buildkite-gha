const fs = require("node:fs");

if (process.env.STATE_phase !== "main") {
  throw new Error(`unexpected phase state: ${process.env.STATE_phase || "missing"}`);
}

fs.writeFileSync(`${process.env.GITHUB_WORKSPACE}/container-runtime-post-ran`, "yes");
fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, "container runtime JavaScript post\n");
