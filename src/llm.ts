export interface Message {
  role: "user" | "assistant";
  content: string;
}

export interface LLMAdapter {
  complete(system: string, messages: Message[], onChunk?: (text: string) => void): Promise<string>;
}

export interface LLMConfig {
  provider?: "anthropic" | "gemini" | "bedrock" | "claude-cli" | "gemini-cli";
  model: string;
  apiKey?: string;
  region?: string;
  accessKeyId?: string;
  secretAccessKey?: string;
}

export function resolveProvider(
  model: string,
  explicit?: string
): "anthropic" | "gemini" | "bedrock" | "claude-cli" | "gemini-cli" {
  if (explicit) {
    const valid = ["anthropic", "gemini", "bedrock", "claude-cli", "gemini-cli"];
    if (!valid.includes(explicit)) {
      throw new Error(`Invalid provider "${explicit}". Must be one of: ${valid.join(", ")}`);
    }
    return explicit as "anthropic" | "gemini" | "bedrock" | "claude-cli" | "gemini-cli";
  }
  if (model.startsWith("claude")) return "anthropic";
  if (model.startsWith("gemini")) return "gemini";
  if (model.includes("anthropic.") || model.startsWith("us.") || model.startsWith("eu."))
    return "bedrock";
  throw new Error(
    `Cannot infer provider for model "${model}". Set KNOMIT_LLM_PROVIDER or specify provider explicitly.`
  );
}

export function cliExists(name: string): boolean {
  try {
    const result = Bun.spawnSync(["which", name]);
    return result.exitCode === 0;
  } catch {
    return false;
  }
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
    case "claude-cli":
      return createClaudeCliAdapter();
    case "gemini-cli":
      return createGeminiCliAdapter();
  }
}

function createAnthropicAdapter(config: LLMConfig): LLMAdapter {
  const apiKey = config.apiKey ?? process.env.ANTHROPIC_API_KEY;
  if (!apiKey) throw new Error("ANTHROPIC_API_KEY is required for Anthropic provider");
  const model = config.model;

  return {
    async complete(system: string, messages: Message[], onChunk?: (text: string) => void): Promise<string> {
      const body: Record<string, unknown> = {
        model,
        max_tokens: 8192,
        system,
        messages: messages.map((m) => ({
          role: m.role,
          content: m.content,
        })),
      };
      if (onChunk) {
        body.stream = true;
      }

      const resp = await fetch("https://api.anthropic.com/v1/messages", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "x-api-key": apiKey,
          "anthropic-version": "2023-06-01",
        },
        body: JSON.stringify(body),
      });
      if (!resp.ok) {
        const text = await resp.text();
        throw new Error(`Anthropic API error ${resp.status}: ${text}`);
      }

      if (onChunk) {
        let result = "";
        const reader = resp.body!.getReader();
        const decoder = new TextDecoder();
        let buffer = "";
        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          const lines = buffer.split("\n");
          buffer = lines.pop()!;
          for (const line of lines) {
            if (!line.startsWith("data: ")) continue;
            const payload = line.slice(6).trim();
            if (payload === "[DONE]") continue;
            try {
              const event = JSON.parse(payload);
              if (event.type === "content_block_delta" && event.delta?.text) {
                result += event.delta.text;
                onChunk(event.delta.text);
              }
            } catch {
              // skip unparseable lines
            }
          }
        }
        return result;
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
    async complete(system: string, messages: Message[], onChunk?: (text: string) => void): Promise<string> {
      const contents = messages.map((m) => ({
        role: m.role === "assistant" ? "model" : "user",
        parts: [{ text: m.content }],
      }));

      const endpoint = onChunk ? "streamGenerateContent" : "generateContent";
      const resp = await fetch(
        `https://generativelanguage.googleapis.com/v1beta/models/${model}:${endpoint}?key=${apiKey}`,
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

      if (onChunk) {
        const reader = resp.body!.getReader();
        const decoder = new TextDecoder();
        let accumulated = "";
        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          const chunk = decoder.decode(value, { stream: true });
          accumulated += chunk;
          onChunk(chunk);
        }

        // Gemini streams a JSON array — try parsing as array first, fall back to single object
        let parsed: any;
        try {
          parsed = JSON.parse(accumulated);
        } catch {
          throw new Error("Failed to parse Gemini streaming response");
        }
        const items = Array.isArray(parsed) ? parsed : [parsed];
        const texts: string[] = [];
        for (const item of items) {
          if (item.candidates?.[0]?.content?.parts) {
            for (const part of item.candidates[0].content.parts) {
              if (part.text) texts.push(part.text);
            }
          }
        }
        return texts.join("");
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
    async complete(system: string, messages: Message[], onChunk?: (text: string) => void): Promise<string> {
      const action = onChunk ? "invoke-with-response-stream" : "invoke";
      const url = `https://bedrock-runtime.${region}.amazonaws.com/model/${encodeURIComponent(model)}/${action}`;
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

      if (onChunk) {
        const reader = resp.body!.getReader();
        const decoder = new TextDecoder();
        let accumulated = "";
        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          const chunk = decoder.decode(value, { stream: true });
          accumulated += chunk;
          onChunk(chunk);
        }

        // Try to parse accumulated text for content_block_delta events
        let result = "";
        const lines = accumulated.split("\n");
        for (const line of lines) {
          if (!line.startsWith("data: ")) continue;
          const payload = line.slice(6).trim();
          if (payload === "[DONE]") continue;
          try {
            const event = JSON.parse(payload);
            if (event.type === "content_block_delta" && event.delta?.text) {
              result += event.delta.text;
            }
          } catch {
            // skip unparseable lines
          }
        }

        // Fall back to single response parsing if no deltas found
        if (!result) {
          try {
            const data = JSON.parse(accumulated) as {
              content: Array<{ type: string; text: string }>;
            };
            const textBlock = data.content.find((c: { type: string }) => c.type === "text");
            if (textBlock) return textBlock.text;
          } catch {
            // could not parse as single response either
          }
          throw new Error("No text in Bedrock streaming response");
        }
        return result;
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

function createClaudeCliAdapter(): LLMAdapter {
  return {
    async complete(system: string, messages: Message[], onChunk?: (text: string) => void): Promise<string> {
      const userContent = messages
        .filter((m) => m.role === "user")
        .map((m) => m.content)
        .join("\n\n");

      const proc = Bun.spawn(
        ["claude", "-p", "--system", system, "--output-format", "text"],
        { stdin: new Blob([userContent]), stdout: "pipe", stderr: "pipe" }
      );

      let result = "";
      const reader = proc.stdout.getReader();
      const decoder = new TextDecoder();
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        const chunk = decoder.decode(value, { stream: true });
        result += chunk;
        if (onChunk) onChunk(chunk);
      }

      const exitCode = await proc.exited;
      if (exitCode !== 0) {
        const stderr = await new Response(proc.stderr).text();
        throw new Error(`claude CLI exited with code ${exitCode}: ${stderr}`);
      }

      return result;
    },
  };
}

function createGeminiCliAdapter(): LLMAdapter {
  return {
    async complete(system: string, messages: Message[], onChunk?: (text: string) => void): Promise<string> {
      const userContent = messages
        .filter((m) => m.role === "user")
        .map((m) => m.content)
        .join("\n\n");

      const stdinContent = system + "\n\n" + userContent;

      const proc = Bun.spawn(
        ["gemini"],
        { stdin: new Blob([stdinContent]), stdout: "pipe", stderr: "pipe" }
      );

      let result = "";
      const reader = proc.stdout.getReader();
      const decoder = new TextDecoder();
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        const chunk = decoder.decode(value, { stream: true });
        result += chunk;
        if (onChunk) onChunk(chunk);
      }

      const exitCode = await proc.exited;
      if (exitCode !== 0) {
        const stderr = await new Response(proc.stderr).text();
        throw new Error(`gemini CLI exited with code ${exitCode}: ${stderr}`);
      }

      return result;
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
    | "claude-cli"
    | "gemini-cli"
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
