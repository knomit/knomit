//go:build !windows

package embeddings

// annotateORTInitError is a no-op off Windows: the VC++ redistributable
// requirement it explains is a Windows-only property of the prebuilt
// onnxruntime binaries.
func annotateORTInitError(err error) error { return err }
