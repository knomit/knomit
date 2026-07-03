import { definePluginEntry } from "openclaw/plugin-sdk/plugin-entry";
import { registerKnomit } from "./register.mjs";

export default definePluginEntry({
  id: "knomit",
  name: "knomit",
  description: "knomit knowledge base tools, proxied through knomit-bridge.",
  register(api) { registerKnomit(api); },
});
