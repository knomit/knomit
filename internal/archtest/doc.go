// Package archtest holds architecture-boundary guard tests — assertions about
// the shape of the build graph rather than runtime behavior. It has no
// production code and is imported by nothing, so it never ships in any binary;
// its tests run in CI to prevent forbidden dependencies from creeping in.
package archtest
