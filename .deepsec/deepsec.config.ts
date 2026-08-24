import { generatedMatchersPlugin } from "./generated-matchers.js";

export default {
  projects: [
    { id: "buildkite-gha", root: ".." },
    // <deepsec:projects-insert-above>
  ],
  plugins: [generatedMatchersPlugin],
};
