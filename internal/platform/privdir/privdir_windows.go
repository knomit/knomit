//go:build windows

package privdir

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"github.com/rs/zerolog/log"
	"golang.org/x/sys/windows"
)

// ensure creates path (os.MkdirAll's mode is ignored on Windows) and, unless
// its DACL is already protected, replaces the DACL with
//
//	D:P(A;OICI;FA;;;<current user>)(A;OICI;FA;;;SY)
//
// WHAT EACH PART IS FOR.
//   - The current user must be in the DACL. An owner not granted anything
//     keeps only the implicit READ_CONTROL and WRITE_DAC: they could fix the
//     DACL, but could not read their own data until they did.
//   - SYSTEM, so that services, backup and antivirus still work.
//   - Administrators are left out on purpose. They can take ownership
//     whatever the DACL says, so no DACL defends against a local admin, and
//     granting them access would only widen it for nothing.
//   - OICI (object and container inherit) makes every existing and future
//     child inherit the same two ACEs, temp files included. That is what
//     makes the temp+rename write pattern safe: the rename carries the temp
//     file's DACL onto the target, and the temp file inherited this one.
//   - P (protected) stops the parent's ACEs from flowing in. It has to be
//     passed as PROTECTED_DACL_SECURITY_INFORMATION as well: measured, the
//     "P" in the SDDL alone is ignored by SetNamedSecurityInfo, and the
//     inherited ACEs merge back in.
//
// A DACL that is ALREADY protected is left alone. It was set deliberately,
// by an earlier boot or by the user, and a boot that overwrote a user's
// explicit choice would break whatever they set it for. It is still read:
// a protected DACL that grants anyone but this user and SYSTEM is reported,
// since "protected" says nothing about "private".
//
// Nothing is rewritten when the process runs as a SERVICE account (see
// isServiceSID). The DACL would then grant the service and SYSTEM only, and
// lock out the interactive user whose data root it is: the one real lockout
// this function could cause.
//
// An owner other than this user, SYSTEM or Administrators is reported
// whatever else happens. An owner keeps an implicit WRITE_DAC and can grant
// themselves access back at any time, so a data root pre-created by another
// local user (a KNOMIT_HOME under C:\, made before this user's first boot)
// is not private to this user, whatever its DACL says.
//
// Any failure after creation (FAT or exFAT with no ACLs at all, a share, no
// WRITE_DAC on a directory owned by someone else) is a warning, not an error;
// see Ensure.
func ensure(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := protect(path); err != nil {
		log.Warn().Err(err).Str("path", path).
			Msg("could not make the data directory private to this user; its files keep whatever access the parent directory grants")
	}
	return nil
}

// The well-known SIDs the checks below are made of, in one place.
const (
	sidSystem          = "S-1-5-18"     // NT AUTHORITY\SYSTEM
	sidLocalService    = "S-1-5-19"     // NT AUTHORITY\LOCAL SERVICE
	sidNetworkService  = "S-1-5-20"     // NT AUTHORITY\NETWORK SERVICE
	sidAdministrators  = "S-1-5-32-544" // BUILTIN\Administrators
	sidVirtualServices = "S-1-5-80-"    // prefix of NT SERVICE\<name> virtual accounts
)

// isServiceSID reports whether sid is a service account: SYSTEM, LOCAL
// SERVICE, NETWORK SERVICE, or a per-service virtual account.
func isServiceSID(sid string) bool {
	switch sid {
	case sidSystem, sidLocalService, sidNetworkService:
		return true
	}
	return strings.HasPrefix(sid, sidVirtualServices)
}

// foreignGrants lists who a DACL lets in besides me and SYSTEM. An ACE whose
// SID Inspect cannot read counts as foreign, and so does a NULL DACL, which
// grants everyone everything. Deny ACEs only take access away, so they are
// not listed.
func foreignGrants(in Inspection, me string) []string {
	if in.NullDACL {
		return []string{"everyone (NULL DACL)"}
	}
	var out []string
	for _, a := range in.ACEs {
		switch {
		case a.SID == "":
			out = append(out, "an ACE of a type whose SID cannot be read")
		case a.Allow && a.SID != me && a.SID != sidSystem:
			out = append(out, a.SID)
		}
	}
	return out
}

// protect is ensure's DACL half. It logs what it declines to do, and returns
// what went wrong for ensure's warning.
func protect(path string) error {
	sid, err := ownSID()
	if err != nil {
		return err
	}
	if isServiceSID(sid) {
		log.Warn().Str("path", path).Str("sid", sid).
			Msg("running as a service account; not rewriting the data root's DACL")
		return nil
	}
	// OWNER and DACL need only READ_CONTROL, so this works without WRITE_DAC.
	in, err := Inspect(path)
	if err != nil {
		return fmt.Errorf("read owner and DACL: %w", err)
	}
	if in.Owner != sid && in.Owner != sidSystem && in.Owner != sidAdministrators {
		log.Warn().Str("path", path).Str("owner", in.Owner).
			Msg("data root is owned by another account, which can always grant itself access to it; it is not private to you")
	}
	if in.Protected {
		if foreign := foreignGrants(in, sid); len(foreign) > 0 {
			log.Warn().Str("path", path).Strs("grants", foreign).
				Msgf("data root DACL is protected and grants %s; leaving it as set, but this is not private to you", strings.Join(foreign, ", "))
		}
		return nil
	}
	want, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + sid + ")(A;OICI;FA;;;SY)")
	if err != nil {
		return fmt.Errorf("build DACL: %w", err)
	}
	dacl, _, err := want.DACL()
	if err != nil {
		return fmt.Errorf("build DACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("set DACL: %w", err)
	}
	return nil
}

// ownSID is the current process's user SID, as a string.
//
// It duplicates internal/auth's ownSID (local_windows.go) on purpose: this
// package is in the platform tier, which may not import knomit packages
// (test/archtest TestPlatformKnowsNothingAboutKnomit), and the twin is three
// lines of OS call.
func ownSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("GetTokenUser for the current process: %w", err)
	}
	return u.User.Sid.String(), nil
}

// Inspection is what Inspect reads back from a file or directory's security
// descriptor.
type Inspection struct {
	// Protected is SE_DACL_PROTECTED: the parent's ACEs do not flow in.
	Protected bool
	// Owner is the owner's SID as a string.
	Owner string
	// NullDACL means there is no DACL at all, which Windows reads as full
	// access for everyone; ACEs is then empty and must not be read as "no one".
	NullDACL bool
	// ACEs is the DACL in order.
	ACEs []ACE
}

// ACE is one access-control entry of a DACL.
type ACE struct {
	// SID is the trustee as a string, e.g. "S-1-5-18" for SYSTEM. It is empty
	// for an ACE type other than plain allow or deny, whose SID is not at the
	// same offset; a file-system DACL does not normally carry one.
	SID string
	// Mask is the access mask as stored. A generic right in SDDL is mapped
	// to specific rights when set, so "GA" on a file reads back as FA
	// (FILE_ALL_ACCESS, 0x1f01ff).
	Mask uint32
	// Flags is the ACE header's flags (OBJECT_INHERIT_ACE, INHERITED_ACE, …).
	Flags uint8
	// Inherited is Flags&INHERITED_ACE: the ACE came from a parent.
	Inherited bool
	// Allow is true for an access-allowed ACE, false for any other type.
	Allow bool
}

// Inspect reads path's owner and DACL. It exists for tests and diagnostics
// that must assert what a directory or file actually grants, which Go's
// os.FileMode cannot say on Windows (every writable file reads 0666).
func Inspect(path string) (Inspection, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return Inspection{}, err
	}
	var in Inspection
	control, _, err := sd.Control()
	if err != nil {
		return Inspection{}, err
	}
	in.Protected = control&windows.SE_DACL_PROTECTED != 0
	owner, _, err := sd.Owner()
	if err != nil {
		return Inspection{}, err
	}
	in.Owner = owner.String()
	dacl, _, err := sd.DACL()
	if err != nil {
		return Inspection{}, err
	}
	if dacl == nil {
		in.NullDACL = true
		return in, nil
	}
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return Inspection{}, fmt.Errorf("ACE %d: %w", i, err)
		}
		e := ACE{
			Mask:      uint32(ace.Mask),
			Flags:     ace.Header.AceFlags,
			Inherited: ace.Header.AceFlags&windows.INHERITED_ACE != 0,
			Allow:     ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE,
		}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE, windows.ACCESS_DENIED_ACE_TYPE:
			// The SID is a variable-length structure that starts at the
			// SidStart field, not a value in it: allow and deny ACEs share
			// this layout, and x/sys exposes no accessor, so this is the one
			// unsafe cast. The pointer stays inside sd's buffer, which is
			// live for the whole loop.
			e.SID = (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		}
		in.ACEs = append(in.ACEs, e)
	}
	return in, nil
}
