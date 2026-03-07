import { describe, it, expect } from "bun:test";
import { getVecExtensionUrl, getOnnxModelUrl, extensionFilename } from "./assets";

describe("asset URLs", () => {
  it("returns correct sqlite-vec URL for darwin arm64", () => {
    const url = getVecExtensionUrl("darwin", "arm64");
    expect(url).toContain("sqlite-vec");
    expect(url).toContain("macos-aarch64");
  });

  it("returns correct sqlite-vec URL for linux x64", () => {
    const url = getVecExtensionUrl("linux", "x64");
    expect(url).toContain("linux-x86_64");
  });

  it("returns correct extension filename per platform", () => {
    expect(extensionFilename("darwin")).toBe("vec0.dylib");
    expect(extensionFilename("linux")).toBe("vec0.so");
    expect(extensionFilename("win32")).toBe("vec0.dll");
  });

  it("returns ONNX model URLs", () => {
    const urls = getOnnxModelUrl();
    expect(urls.model).toContain("huggingface.co");
    expect(urls.tokenizer).toContain("huggingface.co");
  });
});
