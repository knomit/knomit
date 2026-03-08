export interface Message {
  role: "user" | "assistant";
  content: string;
}

export interface LLMAdapter {
  complete(system: string, messages: Message[]): Promise<string>;
}

export interface LLMConfig {
  provider?: "anthropic" | "gemini" | "bedrock";
  model: string;
  apiKey?: string;
  region?: string;
  accessKeyId?: string;
  secretAccessKey?: string;
}

export function resolveProvider(
  model: string,
  explicit?: string
): "anthropic" | "gemini" | "bedrock" {
  if (explicit) return explicit as "anthropic" | "gemini" | "bedrock";
  if (model.startsWith("claude")) return "anthropic";
  if (model.startsWith("gemini")) return "gemini";
  if (model.includes("anthropic.") || model.startsWith("us.") || model.startsWith("eu."))
    return "bedrock";
  throw new Error(
    `Cannot infer provider for model "${model}". Set KNOMIT_LLM_PROVIDER or specify provider explicitly.`
  );
}

export function createAdapter(config: LLMConfig): LLMAdapter {
  const provider = resolveProvider(config.model, config.provider);
  switch (provider) {
    case "anthropic":
      return createAnthropicAdapter(config);
    case "gemini":
      return createGeminiAdapter(config);
    case "bedrock":
      return createBedrockAdapter(config);
  }
}

function createAnthropicAdapter(config: LLMConfig): LLMAdapter {
  const apiKey = config.apiKey ?? process.env.ANTHROPIC_API_KEY;
  if (!apiKey) throw new Error("ANTHROPIC_API_KEY is required for Anthropic provider");
  const model = config.model;

  return {
    async complete(system: string, messages: Message[]): Promise<string> {
      const resp = await fetch("https://api.anthropic.com/v1/messages", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "x-api-key": apiKey,
          "anthropic-version": "2023-06-01",
        },
        body: JSON.stringify({
          model,
          max_tokens: 8192,
          system,
          messages: messages.map((m) => ({
            role: m.role,
            content: m.content,
          })),
        }),
      });
      if (!resp.ok) {
        const body = await resp.text();
        throw new Error(`Anthropic API error ${resp.status}: ${body}`);
      }
      const data = (await resp.json()) as {
        content: Array<{ type: string; text: string }>;
      };
      const textBlock = data.content.find((c) => c.type === "text");
      if (!textBlock) throw new Error("No text in Anthropic response");
      return textBlock.text;
    },
  };
}

function createGeminiAdapter(config: LLMConfig): LLMAdapter {
  const apiKey = config.apiKey ?? process.env.GOOGLE_AI_API_KEY;
  if (!apiKey) throw new Error("GOOGLE_AI_API_KEY is required for Gemini provider");
  const model = config.model;

  return {
    async complete(system: string, messages: Message[]): Promise<string> {
      const contents = messages.map((m) => ({
        role: m.role === "assistant" ? "model" : "user",
        parts: [{ text: m.content }],
      }));
      const resp = await fetch(
        `https://generativelanguage.googleapis.com/v1beta/models/${model}:generateContent?key=${apiKey}`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            system_instruction: { parts: [{ text: system }] },
            contents,
          }),
        }
      );
      if (!resp.ok) {
        const body = await resp.text();
        throw new Error(`Gemini API error ${resp.status}: ${body}`);
      }
      const data = (await resp.json()) as {
        candidates: Array<{ content: { parts: Array<{ text: string }> } }>;
      };
      return data.candidates[0].content.parts[0].text;
    },
  };
}

function createBedrockAdapter(config: LLMConfig): LLMAdapter {
  const region = config.region ?? process.env.AWS_REGION ?? "us-east-1";
  const accessKeyId = config.accessKeyId ?? process.env.AWS_ACCESS_KEY_ID;
  const secretAccessKey = config.secretAccessKey ?? process.env.AWS_SECRET_ACCESS_KEY;
  if (!accessKeyId || !secretAccessKey)
    throw new Error("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are required for Bedrock provider");
  const model = config.model;

  return {
    async complete(system: string, messages: Message[]): Promise<string> {
      const url = `https://bedrock-runtime.${region}.amazonaws.com/model/${encodeURIComponent(model)}/invoke`;
      const payload = JSON.stringify({
        anthropic_version: "bedrock-2023-05-31",
        max_tokens: 8192,
        system,
        messages: messages.map((m) => ({
          role: m.role,
          content: m.content,
        })),
      });

      const headers = await signAwsRequest(
        "POST",
        url,
        payload,
        region,
        accessKeyId,
        secretAccessKey
      );

      const resp = await fetch(url, {
        method: "POST",
        headers: { ...headers, "Content-Type": "application/json" },
        body: payload,
      });
      if (!resp.ok) {
        const body = await resp.text();
        throw new Error(`Bedrock API error ${resp.status}: ${body}`);
      }
      const data = (await resp.json()) as {
        content: Array<{ type: string; text: string }>;
      };
      const textBlock = data.content.find((c) => c.type === "text");
      if (!textBlock) throw new Error("No text in Bedrock response");
      return textBlock.text;
    },
  };
}

/** Minimal AWS Signature V4 signing for Bedrock requests. */
async function signAwsRequest(
  method: string,
  url: string,
  body: string,
  region: string,
  accessKeyId: string,
  secretAccessKey: string
): Promise<Record<string, string>> {
  const encoder = new TextEncoder();
  const parsed = new URL(url);
  const now = new Date();
  const amzDate = now.toISOString().replace(/[-:]/g, "").replace(/\.\d{3}/, "");
  const dateStamp = amzDate.slice(0, 8);
  const service = "bedrock-runtime";

  const sha256 = async (data: string | Uint8Array): Promise<string> => {
    const buf = typeof data === "string" ? encoder.encode(data) : data;
    const hash = new Bun.CryptoHasher("sha256").update(buf).digest("hex");
    return hash;
  };

  const hmac = async (
    key: string | Uint8Array,
    data: string
  ): Promise<Uint8Array> => {
    const keyBuf = typeof key === "string" ? encoder.encode(key) : key;
    const hasher = new Bun.CryptoHasher("sha256", keyBuf);
    hasher.update(encoder.encode(data));
    return new Uint8Array(hasher.digest());
  };

  const payloadHash = await sha256(body);
  const canonicalHeaders =
    `host:${parsed.host}\nx-amz-date:${amzDate}\n`;
  const signedHeaders = "host;x-amz-date";
  const canonicalRequest = [
    method,
    parsed.pathname,
    "",
    canonicalHeaders,
    signedHeaders,
    payloadHash,
  ].join("\n");

  const credentialScope = `${dateStamp}/${region}/${service}/aws4_request`;
  const stringToSign = [
    "AWS4-HMAC-SHA256",
    amzDate,
    credentialScope,
    await sha256(canonicalRequest),
  ].join("\n");

  const kDate = await hmac(`AWS4${secretAccessKey}`, dateStamp);
  const kRegion = await hmac(kDate, region);
  const kService = await hmac(kRegion, service);
  const kSigning = await hmac(kService, "aws4_request");

  const signatureHasher = new Bun.CryptoHasher("sha256", kSigning);
  signatureHasher.update(encoder.encode(stringToSign));
  const signature = signatureHasher.digest("hex");

  return {
    "X-Amz-Date": amzDate,
    Authorization: `AWS4-HMAC-SHA256 Credential=${accessKeyId}/${credentialScope}, SignedHeaders=${signedHeaders}, Signature=${signature}`,
    "x-amz-content-sha256": payloadHash,
  };
}

/** Resolve LLM config from environment variables. */
export function configFromEnv(): LLMConfig {
  const provider = process.env.KNOMIT_LLM_PROVIDER as
    | "anthropic"
    | "gemini"
    | "bedrock"
    | undefined;
  const model = process.env.KNOMIT_LLM_MODEL ?? "claude-sonnet-4-6";
  return {
    provider,
    model,
    apiKey:
      provider === "gemini"
        ? process.env.GOOGLE_AI_API_KEY
        : process.env.ANTHROPIC_API_KEY,
    region: process.env.AWS_REGION,
    accessKeyId: process.env.AWS_ACCESS_KEY_ID,
    secretAccessKey: process.env.AWS_SECRET_ACCESS_KEY,
  };
}
