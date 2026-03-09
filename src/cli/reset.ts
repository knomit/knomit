import { defineCommand } from "citty";
import { globalArgs } from "./args";

export default defineCommand({
  meta: {
    name: "reset",
    description: "Wipe the repository and search index",
  },
  args: {
    ...globalArgs,
  },
  async run({ args }) {
    const { reset } = await import("../bootstrap.js");
    await reset(args.repo, args["cache-dir"]);
  },
});
