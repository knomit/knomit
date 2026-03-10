import { parse as parseYaml } from "yaml";
import { z } from "zod";

const StepSchema = z.object({
  mode: z.enum(["prune", "distill"]),
  model: z.string().optional(),
  prompt: z.string().optional().default(""),
  max_depth: z.number().int().min(1).optional().default(1),
  umap_dimensions: z.number().int().min(2).optional().default(5),
  min_cluster_size: z.number().int().min(2).optional().default(3),
});

const ScopeSchema = z.object({
  domain: z.array(z.string()).optional().default([]),
  entities: z.array(z.string()).optional().default([]),
  search: z.array(z.string()).optional().default([]),
  path: z.string().optional().default(""),
});

const RecipeSchema = z.object({
  name: z.string().min(1),
  prompt: z.string().optional().default(""),
  scope: ScopeSchema.optional(), // undefined = auto-discovery mode
  auto_merge: z.boolean().optional().default(false),
  steps: z.array(StepSchema).min(1),
});

export type Recipe = z.infer<typeof RecipeSchema>;
export type RecipeStep = z.infer<typeof StepSchema>;

export function parseRecipe(raw: string): Recipe {
  const parsed = parseYaml(raw);
  return RecipeSchema.parse(parsed);
}
