const fs = require("node:fs");

fs.appendFileSync(process.env.GITHUB_STATE, "pre=ready\n");
console.log("lifecycle:pre");
