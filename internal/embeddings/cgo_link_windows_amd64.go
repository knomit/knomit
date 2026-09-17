// Windows linking for the daulet/tokenizers static library.
//
// The -L path is the per-platform native-lib dir, as on darwin and linux. The
// rest of the line exists because of LINK ORDER.
//
// daulet/tokenizers declares its own windows LDFLAGS (-ltokenizers -lm
// -lstdc++ -lws2_32 -luserenv, added upstream in PR 61). That set is
// incomplete against a modern Rust standard library, which calls the NT native
// API directly — NtReadFile and RtlNtStatusToDosError, from
// std/sys/pal/windows/handle.rs, are the ones this build trips over — and
// those live in ntdll.
//
// The Go linker builds the final gcc command with THIS package's LDFLAGS ahead
// of the imported package's, and ld resolves archives strictly left to right:
// a library satisfies only the undefined symbols recorded to its left. A bare
// -lntdll therefore lands BEFORE -ltokenizers, is scanned while nothing needs
// it, and contributes nothing:
//
//	-L<dir> -lntdll ... -ltokenizers -lm -lstdc++ -lws2_32 -luserenv
//	        ^ scanned here            ^ needs NtReadFile — too late
//
// Naming -ltokenizers again here, immediately followed by -lntdll, puts a copy
// of the archive to the LEFT of ntdll, so the NT symbols resolve wherever the
// linker splices these flags in. Removing the repeated -ltokenizers and
// re-linking brings back "undefined reference to NtReadFile" and friends.
//
// This only fails at LINK time, so `go build ./internal/embeddings` will not
// catch a regression here — it compiles the package without linking it. Build
// a main package (`go build .`) to exercise it.
//
// -lbcrypt is deliberately absent: the Rust runtime reaches randomness through
// ProcessPrng, which libtokenizers.a resolves itself, and the binary links
// without it.

package embeddings

// #cgo LDFLAGS: -L${SRCDIR}/../../dist/windows-amd64/lib -ltokenizers -lntdll
import "C"
